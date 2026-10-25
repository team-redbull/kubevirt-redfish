# kubevirt-redfish at scale: every change in this fork, and how to upstream it

This repository is a fork of [kubevirt/redfish-controller](https://github.com/kubevirt/redfish-controller).
`main` is upstream `main` (`2e38e46`, after #34 middleware removal) with the five open upstream PRs merged in, each as a
merge commit of its PR branch, plus two fork-only files. Nothing else differs.

| On `main` | Upstream PR? | Change |
|-----------|--------------|--------|
| Merge of `fix-worker-stop` | [#29](https://github.com/kubevirt/redfish-controller/pull/29) | `Worker.Stop` no longer blocks forever, so shutdown no longer waits for SIGKILL |
| Merge of `client-go-rate-limits` | [#30](https://github.com/kubevirt/redfish-controller/pull/30) | Change 1: configurable client-go rate limit, default unchanged at 5/10 |
| Merge of `task-worker-pool` | [#31](https://github.com/kubevirt/redfish-controller/pull/31) | Changes 2 + 4: round-robin worker selection, `server.workers` |
| Merge of `vm-informer-cache` | [#32](https://github.com/kubevirt/redfish-controller/pull/32) | Change 3: informer cache for VM/VMI reads |
| Merge of `fix-vm-write-race` | [#33](https://github.com/kubevirt/redfish-controller/pull/33) | Change 5: serialized, version-checked VM updates, so a concurrent insert and boot override cannot drop the `cdrom0` disk |
| Fork-only commit | **no** | `.github/workflows/scalefix-image.yml`: builds this fork's image on push |
| Fork-only commit | **no** | This file |

Because the PR branches are merged as they are, `main` keeps up with upstream by merging
`upstream/main`; once a PR is merged upstream, its commit is already here. If a PR branch
changes during review, merge that branch into `main` again. Never build a PR from
`git diff upstream/main..main`: that diff also carries the fork-only files and the other PRs.

Until 2026-10-22 `main` was upstream `b40c637` plus the fork's own commits (`484d07d`,
`f878015`, `fe33538`, `c7c59a7`, ...), which predated upstream #26 and #27. That history was
replaced by the layout above; it is kept on the branch `main-pre-rebuild`.

`main` is the only working branch of this fork. Each push to it builds
`ghcr.io/team-redbull/kubevirt-redfish:main-scalefix` and an immutable
`:sha-<commit>` tag (`.github/workflows/scalefix-image.yml`).

History: the changes were first written against upstream's `kubevirt-1.8-tp` branch
(commit `44a671e`, the source of the Red Hat `kubevirt-redfish-rhel9:v4.22` image) on a
branch `kubevirt-1.8-tp-scalefix`, which has since been deleted. Its images remain in
GHCR as `:v4.22-scalefix` and `:sha-f1183ac418e090d255011e634ff82a0419546519`.

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

## Change 1: configurable client-go rate limit

The fork does **not** raise the limit by default; it makes it configurable so an
operator can raise it for a large deployment. (An earlier version of the fork hardcoded
100/200; review moved the default back to client-go's own.)

- `pkg/config/config.go`: `KubeVirtConfig` gains `QPS float32` and `Burst int`
  (`kubevirt.qps`, `kubevirt.burst`). Defaults are `DefaultKubeClientQPS` /
  `DefaultKubeClientBurst`, defined as client-go's `rest.DefaultQPS` (5) and
  `rest.DefaultBurst` (10), so leaving them unset changes nothing. Zero means "use the
  default"; negatives are rejected by validation.
- Environment variables need no extra code: viper already maps `KUBEVIRT_REDFISH_*`,
  so `KUBEVIRT_REDFISH_KUBEVIRT_QPS` / `KUBEVIRT_REDFISH_KUBEVIRT_BURST` override the
  file. This is the same mechanism every other setting uses, which is why it is
  preferred over a separate flag or a new env-only variable.
- `pkg/kubevirt/client.go`: `NewClient` calls the pure helper `kubeClientRateLimits`,
  which reads the values through a small interface, `GetKubeClientRateLimits()`, the
  same way the client already reads `GetDataVolumeConfig()`. Without a config, or for
  a field left at zero, client-go's defaults apply. The value in use is logged at
  startup: `Kubernetes client rate limits: qps=... burst=...`.
- Helm: `kubevirt.qps` / `kubevirt.burst` in `values.yaml` default to `0`, which leaves
  them out of the rendered `config.yaml`; set them in values, or set the env vars via
  `deployment.container.env`. `values-example.yaml` shows 100/200 for a large
  deployment.

**Values for many concurrent BMHs.** With N hosts in flight the slowest request waits
about `12 × N / qps` seconds, so 5/10 saturates at ~15 BMHs. 100/200 was what fixed
the original problem, and it is safe for the API server: 100 requests/second at ~20 ms
each is about two concurrent requests, against an OpenShift API server that allows
thousands in flight. `Waited for Xs due to client-side throttling` in the log means
the limit is too low for the load. For example:

```yaml
kubevirt:
  qps: 100
  burst: 200
```

or `KUBEVIRT_REDFISH_KUBEVIRT_QPS=100` / `KUBEVIRT_REDFISH_KUBEVIRT_BURST=200`.

## Change 2: round-robin worker selection

`pkg/server/task_manager.go`, `getAvailableWorker`: an atomic counter picks a different
starting worker for each job, then scans forward for one with capacity. Before, the scan
always started at worker 1, which has 10 queue slots, so every job landed there.

## Change 3: an in-memory cache for VM and VMI reads

The largest change: `pkg/kubevirt/cache.go` (new, plain client-go informers) and small edits in
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
- *A read that builds the response to our own write goes live.* The boot-device `PATCH`
  handler writes the VM and then reads it back for its response body; that read uses
  `GetVMBootOptionsLive`, so it never returns the pre-write boot options. No other write
  path reads through a cached getter.

  (The first revision of #32 instead wrapped the dynamic client in a write tracker that
  bypassed the cache after each of this process's writes until the informer caught up.
  Review asked for less machinery, and the revision `bd12d08` removed it; the fork's
  `main` merged that revision on 2026-10-25.)

**What the cache does not guarantee: writes by anyone else.** A change made outside this
service (`oc edit vm`, KubeVirt's own controllers updating a VMI) reaches the cache when
its watch event arrives, usually within milliseconds and longer when the API server is
overloaded. Until then a cached read returns the previous version, for example the old
CPU count. A direct GET would already see the change. This is the cost of the cache; it
is acceptable because no write path reads from it, so a stale read can make a response
out of date but cannot be written back over someone's change.

**No off switch:** since `bd12d08` the cache is always on; `kubevirt.cache_enabled` and
`KUBEVIRT_REDFISH_KUBEVIRT_CACHE_ENABLED` no longer exist (an old key in a config file is
ignored). The only direct reads are the ones above and those made before an informer has
synced.
**Observability:** `GetMetrics` counts `GetVM.cache` / `GetVM.direct` (and the VMI and
List equivalents). Startup logs `VM cache started for namespaces [...]`.
**RBAC:** the informers need `list` and `watch` on `virtualmachines` and
`virtualmachineinstances`; the Helm chart's ClusterRole already grants both.

Side effect: `ListVMsWithSelector` now returns names sorted (the API server already did,
so callers see no difference), via the new `filterVMNames` helper.

## Change 4: configurable worker pool

`pkg/server/server.go`: the pool was hardcoded to 4 workers; now `server.workers`
(`KUBEVIRT_REDFISH_SERVER_WORKERS`), default `DefaultWorkers` (8), allowed 1-64, zero
means default. Each job is a short burst of API calls (creating the PVC and copy pod for
a virtual media insert; the ISO copy runs in that pod), so this bounds how many inserts
*start* at once. Read once at startup; a config reload does not resize the pool.

## Change 5: VM writes that cannot overwrite each other

Every VM write used to read the VM, edit a copy and send a merge patch of the difference
with no `resourceVersion`, so two concurrent writers silently overwrote each other, and a
merge patch replaces arrays wholesale. When Ironic's `InsertMedia` (run on a task worker)
and its boot-once `PATCH` (run on the HTTP goroutine ~35 ms later) overlapped, the
boot-once patch could write back a `disks` array from before the insert: the VM kept the
`cdrom0` volume without its disk, KubeVirt attached it read-write, and the launcher
crash-looped on the read-only ISO. #33 routes every read-modify-write through `updateVM`,
which holds a per-VM lock in this process and sends the `resourceVersion`, retrying on a
conflict. Writes from other processes are caught by the version check; the lock avoids
needless conflicts between this process's own writers. These writes read live, never from
the cache (change 3).

## Config and chart plumbing

- `pkg/config/config.go`: the new fields, defaults, validation, getters, and the
  constants (see change 1 for the rate limit). `validateKubeVirtConfig` / `validateServerConfig` turn zero into the default
  so a `Config` built in code without viper still validates (an existing test does that).
- `config/config.yaml`, `helm/values.yaml`, `helm/values-example.yaml`: the new keys with
  comments.
- `helm/templates/secret.yaml`: renders `workers`, `qps`, `burst` into
  the mounted `config.yaml`.

## Tests

- `pkg/kubevirt/cache_test.go` (new), against `k8s.io/client-go/dynamic/fake`: a read is
  served from the cache even when the API server's GET is made to fail; a synced cache's
  "not found" is final; reads before the initial sync go direct; the boot-options
  read-back after a write is live; the cache can be restarted with a different
  namespace set, and stopped without deadlocking; `filterVMNames` sorts and filters.
- `pkg/config/config_test.go`: defaults (rate limits 5/10), file and environment
  overrides, rejection of negative values, and zero-means-default for the new keys.
- `pkg/kubevirt/ratelimit_test.go` (new): `kubeClientRateLimits` keeps client-go's
  defaults with no config or zero values, and uses configured values otherwise.

`go.mod` gains one indirect test dependency and `go.sum` two lines (part of #32);
no runtime dependency changes. `gofmt`, `go build ./...`, `go vet ./...`, `go test ./...`
and upstream's `go mod tidy` check all pass.

## Verified on a cluster

Run on OpenShift 4.22 (Kubernetes 1.35) with OpenShift Virtualization 4.22.12, deployed
with this chart under its own ServiceAccount and ClusterRole, with two chassis in two
namespaces and a user restricted to each. API calls were counted from the kube-apiserver
audit log, filtered to the ServiceAccount.

| Check | Result |
|-------|--------|
| Initial sync under the chart's RBAC | one list + watch per namespace for VMs and VMIs; no reflector or `forbidden` errors |
| 100 × `GET /Systems/{id}`, 20 in parallel, rate limit 5/10, cache on | **0** API calls; 5.3s total, 0.75s avg per request (mostly the network round trip to the cluster) |
| Same, `KUBEVIRT_REDFISH_KUBEVIRT_CACHE_ENABLED=false` | **500** VM GETs; 100s total, **19.3s avg**, 21.5s max: the original problem, reproduced |
| Power on, polled every 0.3s | `Off` → `PoweringOn` → `On`, no flapping; matches `oc` |
| Pause / Resume | `Paused` / `On` within ~1s, from the cached VMI |
| ForceOff | `ForceOffInProgress` → `Off` |
| 8 rounds of On / ForceOff on two VMs, 20 reads after each | before the race fix: most reads went direct and a "has not reflected" warning per action; after: **100% cached, 0 warnings** |
| Boot-device `PATCH` (stopped and running VM) | response body and the next reads show the new value; matches the VM's annotations |
| `oc patch` of CPU cores (an external write) | visible through Redfish within ~1s |
| Virtual media insert, then eject | `Inserted` true ~3s after the insert; false on the first read after the eject |
| Pod restart with one VM paused | first reads after readiness report `Paused` / `Off` correctly, from the cache |
| User of chassis A reading or patching a system in chassis B, or an unlabeled VM | 404 |
| Startup log | `Kubernetes client rate limits`, `Task manager started with 8 workers`, `VM cache started for namespaces [...]` |

A second run covered the rate limits, the worker pool and the round-robin dispatch
(one chassis, eight stopped cirros VMs; "env" means the variable set through
`deployment.container.env`, abbreviated after the first row: `KUBEVIRT_QPS` is
`KUBEVIRT_REDFISH_KUBEVIRT_QPS`; every config change was followed by a rollout restart):

| Check | Result |
|-------|--------|
| Defaults | `qps=5 burst=10`, `Task manager started with 8 workers`; `helm template` renders no `qps`/`burst` keys |
| `kubevirt.qps: 100`, `kubevirt.burst: 200`, `server.workers: 3` in values | rendered into the Secret; log `qps=100 burst=200`, 3 workers |
| Env `KUBEVIRT_REDFISH_KUBEVIRT_QPS=50` and `KUBEVIRT_REDFISH_SERVER_WORKERS=5` on top of those file values | env wins: `qps=50 burst=200`, 5 workers |
| Env `KUBEVIRT_QPS=100`, `KUBEVIRT_BURST=200`, file without the keys | `qps=100 burst=200`, 8 workers |
| Env `KUBEVIRT_BURST=200` alone | `qps=5 burst=200` |
| `helm template -f helm/values-example.yaml` | renders `qps: 100`, `burst: 200`, `workers: 8`, `cache_enabled: true` |
| Env `KUBEVIRT_QPS=2.5`, `SERVER_WORKERS=1` | `qps=2.5 burst=10`, 1 worker (fractional QPS works) |
| Env `KUBEVIRT_QPS=0`, `KUBEVIRT_BURST=0`, `server.workers: 64` | `qps=5 burst=10` (zero means default), 64 workers |
| `kubevirt.qps: -1`; `server.workers: 65`; env `KUBEVIRT_BURST=-5` (one at a time) | each rejected: `Configuration validation failed (... must be ...)`, the new pod crash-loops and the previous pod keeps serving |
| Env `KUBEVIRT_QPS=abc` | rejected before validation: `Failed to unmarshal config (... cannot parse 'kubevirt.qps' as float)` |
| 100 × `GET /Systems/{id}`, 20 in parallel, cache off, 5/10 | 505 VM GETs in the audit log; 99.4s total, **19.4s avg**, 21.9s max; 10 `client-side throttling` lines |
| Same at 100/200 | 505 VM GETs; 4.8s total, **0.89s avg**, 1.35s max; 0 throttling lines; peak 155 GETs/s (the burst) |
| 8 × `InsertMedia` POSTed at once, this fork (8 workers, 100/200) | jobs on workers 2, 3, 4, 5, 6, 7, 8, 1 (one each), up to four running at the same time, 127-184ms per job, the whole batch dispatched and done in 0.76s; `Inserted` true after 16-18s (the ISO copy) |
| Same against upstream `b40c637` built the same way (4 workers, hardcoded 5/10) | all 8 jobs on worker 1, strictly one after another, 1.2-1.9s each, 10.3s for the batch; the POSTs themselves took up to 2.1s; `Inserted` after 21-24s |
| 8 × `EjectMedia` at once (both builds) | 200 each (the eject is synchronous: ~1.0s on the fork, 1.3-6.1s upstream); `Inserted` false, PVCs, copy pods and the `cdrom0` disk and volume gone |

Two bugs were found this way and fixed (the cache race above, and the rate-limit log
line, which never appeared because `cmd/main.go` created the client before initializing
the logger).

## Known limits

- Found on the cluster and fixed by #29: `Worker.work` in `pkg/server/task_manager.go`
  never called `wg.Done()`, so `Worker.Stop` blocked forever on shutdown. The pod ignored
  SIGTERM until the kubelet killed it at the end of the 30s grace period, and
  `kubevirtClient.Close()` (which stops the watchers and the cache) never ran.
- Upstream and not fixed here: config hot reload never fires in a pod. The watcher in
  `cmd/main.go` reacts only to Write/Create events named exactly `config.yaml`, but the
  kubelet updates a mounted Secret by swapping the `..data` symlink, which produces no such
  event. After a `helm upgrade` the file in the pod has the new content and the process
  keeps the old config until it restarts. (Watching the directory and reacting to the
  `..data` swap would fix it.)
- Also upstream and not fixed here: `GET /redfish/v1/TaskService/Tasks/{id}` always answers
  404. `handleTask` in `pkg/server/server.go` takes the task ID from `pathParts[4]`, which for
  that route is the literal `Tasks`. The 202 response of `InsertMedia` carries the task in its
  body, and the result can be read from the VM's `VirtualMedia/Cd` (`Inserted`), which is what
  Ironic does; only polling the task itself is impossible. Seen on this fork and on upstream.
- A non-numeric value in `KUBEVIRT_REDFISH_KUBEVIRT_QPS` (or any numeric key) fails at
  unmarshal time with viper's message, not with the validation message; the pod still refuses
  to start, which is the behaviour that matters.
- Because hot reload never fires, `restartVMCache` on a config reload never runs in a
  deployed pod; that path is covered by unit tests only. (`Close` on shutdown runs since #29.)
  A restart with a changed chassis list was verified: the new pod caches all namespaces.
- Default behaviour changes in two places: workers 4 → 8, and reads served from the
  cache. The rate limit stays at client-go's 5/10 unless configured; a deployment with
  many concurrent BMHs has to raise it (change 1). Everything else is opt-in.
- The watchers and the cache are keyed on labels, not on which instance owns a VM; running
  several instances against the same namespace was already unsupported and still is.

## Upstream PRs

Opened on 2026-10-21 against upstream `main` (`590daee`) as five independent PRs, so the
maintainers can review and merge them separately and in any order. Each branch is based
directly on upstream, and this fork's `main` is those five branches merged together:

| PR | Branch (on this fork) | Content |
|----|-----------------------|---------|
| [#29](https://github.com/kubevirt/redfish-controller/pull/29) | `fix-worker-stop` | `defer w.wg.Done()` in `Worker.work` (the shutdown hang under "Known limits") + `pkg/server/worker_test.go` |
| [#30](https://github.com/kubevirt/redfish-controller/pull/30) | `client-go-rate-limits` | Change 1: `kubevirt.qps` / `kubevirt.burst`, `ratelimit_test.go`, logger initialized before the client |
| [#31](https://github.com/kubevirt/redfish-controller/pull/31) | `task-worker-pool` | Changes 2 + 4: round-robin worker selection and `server.workers`, with a new `pkg/server/task_manager_test.go` |
| [#32](https://github.com/kubevirt/redfish-controller/pull/32) | `vm-informer-cache` | Change 3: `cache.go` / `cache_test.go`, the `go mod tidy` lines (the `kubevirt.cache_enabled` switch and the write tracker were dropped in review, `bd12d08`) |
| [#33](https://github.com/kubevirt/redfish-controller/pull/33) | `fix-vm-write-race` | Change 5: every VM read-modify-write goes through `updateVM` (`vm_update.go`), serialized per VM (`vmlock.go`) and sent with `resourceVersion` so a stale write is retried instead of overwriting; fixes the `cdrom0` disk lost when `InsertMedia` and the boot-once `PATCH` race |

Each PR body states the problem, the cause, the fix, the behaviour change and the
evidence: the new unit test failing on upstream `main`, and before/after numbers measured
on this cluster with that branch alone (100 × `GET /Systems/{id}`, 20 in parallel: 19.2 s
average at the default limiter, 0.89 s at 100/200, 0.83 s with the cache at the default
limiter and zero API calls; pod termination 32.5 s on upstream, 3.2 s with #29). Every
branch passed `gofmt`, `go build`, `go vet`, `go test ./...`, `go mod tidy` and
`helm lint`, and the four merged together reproduce this fork's code exactly (checked
with a scratch merge).

Rebase note: #30, #31 and #32 each add the same two test helpers (`minimalConfig`,
`writeConfig`) to `pkg/config/config_test.go`, and all three touch adjacent lines in
`config.go`, `config/config.yaml`, `helm/values.yaml` and `helm/templates/secret.yaml`.
#32 and #33 both add fields to `Client` and to its constructor in `pkg/kubevirt/client.go`.
Whichever lands later needs a short rebase: keep one copy of the helpers, keep both sides
of the adjacent hunks. Git merges the helpers without a conflict into duplicate
declarations, so build after every merge. The merge commits on this fork's `main` show
the resolution.

Race detector: `go test -race ./...` fails on upstream `main` too (`TestServerConcurrentAccess`
panics in the boot-once watcher, and the `Stop` methods of `AdvancedCache`, `JobScheduler`,
`MemoryManager` and the connection pool race with their own goroutines). Upstream CI runs
without `-race`. `pkg/kubevirt` and `pkg/config`, which hold most of the code in these PRs,
pass with `-race`.

Not included in any PR: `.github/workflows/scalefix-image.yml` and this file. The config
hot-reload bug under "Known limits" is mentioned in #29 but not fixed.

