# kubevirt-redfish at scale: every change in this fork, and how to upstream it

This repository is a fork of [kubevirt/redfish-controller](https://github.com/kubevirt/redfish-controller).
`main` is upstream `main` (`b40c637`) plus the commits below. Nothing else differs.

| Commit    | Upstream PR? | Change |
|-----------|--------------|--------|
| `484d07d` | yes | Raise client-go rate limits; round-robin task worker selection |
| `f878015` | yes | Make the rate limits configuration (`kubevirt.qps` / `kubevirt.burst`) |
| `fe33538` | yes | Informer cache for VM/VMI reads; `server.workers`; zero-means-default config |
| (next)    | **no** | `.github/workflows/scalefix-image.yml`: builds this fork's image on push |
| (next)    | **no** | This file |

To see exactly what an upstream PR would contain:

```bash
git diff upstream/main..main -- pkg cmd config helm
```

The branch `kubevirt-1.8-tp-scalefix` carries the same changes on top of upstream's
`kubevirt-1.8-tp` branch (commit `44a671e`, the source of the Red Hat
`kubevirt-redfish-rhel9:v4.22` image), plus a prebuilt static binary in `dist/` for
air-gapped use. It is a deployment branch, not a PR candidate.

## The problem being solved

With about 15 BareMetalHosts provisioning at the same time, Redfish requests took
10-35 seconds. Ironic's set-boot-device `PATCH` ran past the OpenShift route's 30-second
timeout, got a 504, and inspection looped forever.

Two causes:

1. **Every Redfish request fans out into several Kubernetes API calls, and they all
   queue behind one small rate limiter.** The controller used client-go's default of
   5 requests/second, burst 10, a default sized for `kubectl`, not a server. A
   `GET /Systems/{id}` is 6 API calls (one to resolve the system name, then VM ×4 and
   VMI ×1), a boot-device `PATCH` is 12, and Ironic polls every host. With N hosts in
   flight the slowest request waits about `12 × N / 5` seconds: at 15 hosts, ~36s.
2. **The background task dispatcher always picked the first worker**, so jobs that
   should run in parallel ran one after another on worker 1.

## Change 1: raise the client-go rate limit

`pkg/kubevirt/client.go`, `NewClient`: `rest.Config.QPS` / `Burst` set to 100 / 200.

Safe for the API server: 100 requests/second at ~20 ms each is about two concurrent
requests, against an OpenShift API server that allows thousands in flight. The limiter
was never protecting anything here.

## Change 2: round-robin worker selection

`pkg/server/task_manager.go`, `getAvailableWorker`: an atomic counter picks a different
starting worker for each job, then scans forward for one with capacity. Before, the scan
always started at worker 1, which has 10 queue slots, so every job landed there.

## Change 3: make the rate limit configuration

`pkg/config/config.go`: `KubeVirtConfig` gains `QPS float32` and `Burst int`
(`kubevirt.qps`, `kubevirt.burst`). viper already maps `KUBEVIRT_REDFISH_*` environment
variables, so `KUBEVIRT_REDFISH_KUBEVIRT_QPS` / `_BURST` work with no extra code.
Defaults are the named constants `DefaultKubeClientQPS` (100) and
`DefaultKubeClientBurst` (200). Zero means "use the default"; negatives are rejected by
validation.

`NewClient` reads the values through a small interface, `GetKubeClientRateLimits()`,
the same way it already reads `GetDataVolumeConfig()`, so callers that pass no config
keep the defaults. The value in use is logged at startup:
`Kubernetes client rate limits: qps=... burst=...`.

## Change 4: an in-memory cache for VM and VMI reads

The largest change: `pkg/kubevirt/cache.go` (new, ~420 lines) and small edits in
`client.go` and `server.go`.

**What it does.** For every namespace in the chassis config the client keeps a
Kubernetes informer (a watch plus a local copy) of VirtualMachines and one of
VirtualMachineInstances, with `managedFields` stripped. Read-only lookups read the local
copy instead of calling the API server: power state, boot options, CPU, memory, storage,
network, virtual media state, the Systems/Chassis/Managers collections, and the lookup in
`resolveSystemVMandCheckAccess` that runs on every request. The informers start and stop
with the existing watchers (`RestartWatchers` / `Close`) and are rebuilt on config reload.

**Effect, cache warm:**

| Redfish request                     | API calls before | after |
|-------------------------------------|------------------|-------|
| `GET /Systems/{id}`                 | 6                | 0     |
| Boot-device `PATCH /Systems/{id}`   | 12               | 6     |
| Ironic's power poll of a stopped VM | 6                | 0     |

**The three rules that keep cached reads correct** (all in `cache.go`):

- *Anything that reads a VM in order to change it does not use the cache.*
  `SetBootOrder`, `SetBootOnce`, virtual media insert/eject, power actions and the import
  labels still read live, exactly as before. A patch computed from a stale copy could
  overwrite a field written a moment earlier, for example the `cdrom0` disk an insert
  just added. `GetVM` / `GetVMI` are unchanged; the cached variants are `getVMForRead` /
  `getVMIForRead` (exported as `GetVMForRead` for the server package).
- *The cache is either fully trusted or not used.* Until an informer finishes its first
  list, reads for that namespace go to the API server. After that its answer is final,
  including "not found". The alternative, a GET on every miss, would cost a GET on every
  power poll of a stopped VM (which has no VMI), the normal state of a host during
  inspection.
- *Reads skip the cache for 3 seconds after this process writes the object.* A thin
  wrapper around the dynamic client (`trackingDynamicClient`) records every successful
  VM/VMI create, update, patch and delete. So a read that follows our own write, like
  the response body of the boot-device `PATCH` or Ironic re-checking `Inserted` right
  after an eject, cannot see the old state while the informer catches up.

**Off switch:** `kubevirt.cache_enabled: false` (or
`KUBEVIRT_REDFISH_KUBEVIRT_CACHE_ENABLED=false`) restores one API call per lookup.
**Observability:** `GetMetrics` counts `GetVM.cache` / `GetVM.direct` (and the VMI and
List equivalents). Startup logs `VM cache started for namespaces [...]`.
**RBAC:** the informers need `list` and `watch` on `virtualmachines` and
`virtualmachineinstances`; the Helm chart's ClusterRole already grants both.

Side effect: `ListVMsWithSelector` now returns names sorted (the API server already did,
so callers see no difference), via the new `filterVMNames` helper.

## Change 5: configurable worker pool

`pkg/server/server.go`: the pool was hardcoded to 4 workers; now `server.workers`
(`KUBEVIRT_REDFISH_SERVER_WORKERS`), default `DefaultWorkers` (8), allowed 1-64, zero
means default. Each job is a short burst of API calls (creating the PVC and copy pod for
a virtual media insert; the ISO copy runs in that pod), so this bounds how many inserts
*start* at once. Read once at startup; a config reload does not resize the pool.

## Config and chart plumbing

- `pkg/config/config.go`: the new fields, defaults, validation, getters, and the
  constants. `validateKubeVirtConfig` / `validateServerConfig` turn zero into the default
  so a `Config` built in code without viper still validates (an existing test does that).
- `config/config.yaml`, `helm/values.yaml`, `helm/values-example.yaml`: the new keys with
  comments.
- `helm/templates/secret.yaml`: renders `workers`, `qps`, `burst`, `cache_enabled` into
  the mounted `config.yaml`.

## Tests

- `pkg/kubevirt/cache_test.go` (new), against `k8s.io/client-go/dynamic/fake`: a read is
  served from the cache even when the API server's GET is made to fail; a synced cache's
  "not found" is final; a successful write makes reads of that object bypass the cache
  and a failed write does not; the cache can be disabled, restarted with a different
  namespace set, and stopped without deadlocking; `filterVMNames` sorts and filters.
- `pkg/config/config_test.go`: defaults, file and environment overrides, rejection of
  negative values, and zero-means-default for the new keys.

`go.mod` / `go.sum` unchanged. `gofmt`, `go build ./...`, `go vet ./...`, `go test ./...`
all pass.

## Known limits

- The cache is tested against a fake client only; it has not yet run against a real API
  server with the KubeVirt CRDs (initial sync under the real ServiceAccount, live
  `Paused` / `Transitioning` VMI states).
- Default behaviour changes in three places: rate limit 5/10 → 100/200, workers 4 → 8,
  and reads served from the cache. Everything else is opt-in.
- The watchers and the cache are keyed on labels, not on which instance owns a VM; running
  several instances against the same namespace was already unsupported and still is.

## Opening the upstream PR

1. Branch from upstream `main`, cherry-pick `484d07d`, `f878015`, `fe33538` (or squash
   them into one). Do **not** include `.github/workflows/scalefix-image.yml` or this file.
2. Use the "problem" and "change" sections above as the PR description; the before/after
   table and the three cache rules are what reviewers will ask about.
3. Suggested PR title: *Scale to many concurrent BMHs: raise client-go rate limits,
   cache VM/VMI reads, round-robin task workers.*
4. Verification for the PR body:

   ```bash
   go build ./... && go vet ./... && go test ./...
   helm lint helm/ && helm template t helm/ | grep -E 'qps|burst|cache_enabled|workers'
   ```

   On a cluster: the three startup log lines (`Kubernetes client rate limits`,
   `Task manager started with N workers`, `VM cache started for namespaces`), then
   `GetVM.cache` vs `GetVM.direct` on the metrics endpoint while hosts are polling.
   `Waited for Xs due to client-side throttling` in the log means the limit is still too
   low.
