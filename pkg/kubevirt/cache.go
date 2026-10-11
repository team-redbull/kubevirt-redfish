/*
 * This file is part of the KubeVirt Redfish project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2025 KubeVirt Redfish project and its authors.
 *
 */

package kubevirt

// A read-only cache of VirtualMachine and VirtualMachineInstance objects.
//
// Every Redfish request fans out to several Kubernetes API calls, and nearly
// all of them are reads of the same VM/VMI objects (a GET System is five GETs,
// a boot-device PATCH is eleven). With many BareMetalHosts polling at once,
// those reads are what saturate the client-side rate limiter. The informers
// here keep one watch per namespace per resource open instead, so the
// read-only Redfish paths are served from memory.
//
// Only read-only inventory lookups use the cache (see getVMForRead and its
// callers). Every read-modify-write path still reads from the API server,
// because a patch computed from a stale object would clobber fields written
// moments earlier. Two further rules keep cached reads correct:
//
//   - A cache that has not finished its initial sync is not consulted at all,
//     and once it has synced, its answer (including "not found") is
//     authoritative. Falling back to a GET on every miss would cost a GET on
//     every power poll of a stopped VM, which has no VMI, and that is the
//     dominant state of a BMH during inspection and provisioning.
//   - After this process writes a VM or VMI, reads of that object bypass the
//     cache for writeBypassWindow, so a read that follows our own write
//     cannot observe the pre-write state while the informer catches up.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/kubevirt/redfish-controller/pkg/logger"
)

var (
	vmGVR  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	vmiGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
)

// writeBypassWindow is how long reads of a VM/VMI skip the cache after this
// process wrote it. Informer lag on a healthy API server is well under a
// second; Ironic's first power-state poll after a reset comes after one.
const writeBypassWindow = 3 * time.Second

// writeTracker remembers when this process last wrote each VM and VMI.
type writeTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newWriteTracker() *writeTracker {
	return &writeTracker{last: make(map[string]time.Time)}
}

func writeKey(gvr schema.GroupVersionResource, namespace, name string) string {
	return gvr.Resource + "/" + namespace + "/" + name
}

// note records a successful write. Resources other than VM and VMI are ignored.
func (t *writeTracker) note(gvr schema.GroupVersionResource, namespace, name string) {
	if t == nil || (gvr != vmGVR && gvr != vmiGVR) {
		return
	}
	t.mu.Lock()
	t.last[writeKey(gvr, namespace, name)] = time.Now()
	t.mu.Unlock()
}

// recentlyWritten reports whether the object was written inside writeBypassWindow.
func (t *writeTracker) recentlyWritten(gvr schema.GroupVersionResource, namespace, name string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := writeKey(gvr, namespace, name)
	ts, ok := t.last[key]
	if !ok {
		return false
	}
	if time.Since(ts) > writeBypassWindow {
		delete(t.last, key)
		return false
	}
	return true
}

// trackingDynamicClient wraps a dynamic client so that every successful write
// to a VM or VMI is recorded in the writeTracker. Reads pass straight through.
type trackingDynamicClient struct {
	dynamic.Interface
	tracker *writeTracker
}

func newTrackingDynamicClient(inner dynamic.Interface, tracker *writeTracker) dynamic.Interface {
	if inner == nil {
		return nil
	}
	return &trackingDynamicClient{Interface: inner, tracker: tracker}
}

func (d *trackingDynamicClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &trackingNamespaceableResource{
		NamespaceableResourceInterface: d.Interface.Resource(gvr),
		gvr:                            gvr,
		tracker:                        d.tracker,
	}
}

type trackingNamespaceableResource struct {
	dynamic.NamespaceableResourceInterface
	gvr     schema.GroupVersionResource
	tracker *writeTracker
}

func (r *trackingNamespaceableResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &trackingResource{
		ResourceInterface: r.NamespaceableResourceInterface.Namespace(namespace),
		gvr:               r.gvr,
		namespace:         namespace,
		tracker:           r.tracker,
	}
}

type trackingResource struct {
	dynamic.ResourceInterface
	gvr       schema.GroupVersionResource
	namespace string
	tracker   *writeTracker
}

func (r *trackingResource) Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	out, err := r.ResourceInterface.Create(ctx, obj, options, subresources...)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, obj.GetName())
	}
	return out, err
}

func (r *trackingResource) Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	out, err := r.ResourceInterface.Update(ctx, obj, options, subresources...)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, obj.GetName())
	}
	return out, err
}

func (r *trackingResource) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	out, err := r.ResourceInterface.UpdateStatus(ctx, obj, options)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, obj.GetName())
	}
	return out, err
}

func (r *trackingResource) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	out, err := r.ResourceInterface.Patch(ctx, name, pt, data, options, subresources...)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, name)
	}
	return out, err
}

func (r *trackingResource) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	err := r.ResourceInterface.Delete(ctx, name, options, subresources...)
	if err == nil {
		r.tracker.note(r.gvr, r.namespace, name)
	}
	return err
}

// vmCache holds one VM informer and one VMI informer per configured namespace.
type vmCache struct {
	stopCh    chan struct{}
	factories []dynamicinformer.DynamicSharedInformerFactory
	vms       map[string]informers.GenericInformer
	vmis      map[string]informers.GenericInformer
}

// newVMCache starts informers for every distinct, non-empty namespace. It does
// not wait for them to sync; readers check HasSynced per call.
func newVMCache(client dynamic.Interface, namespaces []string) *vmCache {
	vc := &vmCache{
		stopCh: make(chan struct{}),
		vms:    make(map[string]informers.GenericInformer),
		vmis:   make(map[string]informers.GenericInformer),
	}
	seen := make(map[string]bool)
	for _, namespace := range namespaces {
		if namespace == "" || seen[namespace] {
			continue
		}
		seen[namespace] = true

		factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, namespace, nil)
		vmInformer := factory.ForResource(vmGVR)
		vmiInformer := factory.ForResource(vmiGVR)
		// Transforms must be set before the factory starts.
		for _, inf := range []informers.GenericInformer{vmInformer, vmiInformer} {
			if err := inf.Informer().SetTransform(stripManagedFields); err != nil {
				logger.Error("Failed to set informer transform for namespace %s: %v", namespace, err)
			}
		}
		vc.vms[namespace] = vmInformer
		vc.vmis[namespace] = vmiInformer
		vc.factories = append(vc.factories, factory)
		factory.Start(vc.stopCh)
	}
	return vc
}

// stop closes the stop channel first so the informer goroutines exit, then
// waits for them. The other order deadlocks.
func (vc *vmCache) stop() {
	if vc == nil {
		return
	}
	close(vc.stopCh)
	for _, factory := range vc.factories {
		factory.Shutdown()
	}
}

// informer returns the synced informer for gvr in namespace, or false when
// there is none or it has not finished its initial list.
func (vc *vmCache) informer(gvr schema.GroupVersionResource, namespace string) (informers.GenericInformer, bool) {
	if vc == nil {
		return nil, false
	}
	m := vc.vms
	if gvr == vmiGVR {
		m = vc.vmis
	}
	inf, ok := m[namespace]
	if !ok || !inf.Informer().HasSynced() {
		return nil, false
	}
	return inf, true
}

// stripManagedFields drops the managedFields block before an object enters
// the store; it is the largest part of a VM object and nothing here reads it.
func stripManagedFields(obj interface{}) (interface{}, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		u.SetManagedFields(nil)
	}
	return obj, nil
}

// vmCacheEnabled reads kubevirt.cache_enabled from the app config; the cache
// is on unless the config says otherwise.
func (c *Client) vmCacheEnabled() bool {
	if c.appConfig == nil {
		return true
	}
	if cfg, ok := c.appConfig.(interface{ GetVMCacheEnabled() bool }); ok {
		return cfg.GetVMCacheEnabled()
	}
	return true
}

// restartVMCache replaces the running cache with one covering namespaces.
// Called with the watchers on startup and on config reload.
func (c *Client) restartVMCache(namespaces []string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	if c.vmCache != nil {
		c.vmCache.stop()
		c.vmCache = nil
	}
	if !c.vmCacheEnabled() {
		logger.Info("VM cache disabled by configuration; VM/VMI reads go to the API server")
		return
	}
	if c.dynamicClient == nil {
		return
	}
	c.vmCache = newVMCache(c.dynamicClient, namespaces)
	logger.Info("VM cache started for namespaces %v", namespaces)
}

func (c *Client) stopVMCache() {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.vmCache != nil {
		c.vmCache.stop()
		c.vmCache = nil
	}
}

func (c *Client) currentVMCache() *vmCache {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	return c.vmCache
}

// GetVMForRead returns a VM for a read-only purpose. It is served from the
// informer cache when the cache is synced for the namespace and this process
// has not written the VM in the last writeBypassWindow; otherwise it is a
// GetVM. Callers that go on to patch the VM must use GetVM.
func (c *Client) GetVMForRead(namespace, name string) (*kubevirtv1.VirtualMachine, error) {
	return c.getVMForRead(namespace, name)
}

func (c *Client) getVMForRead(namespace, name string) (*kubevirtv1.VirtualMachine, error) {
	start := time.Now()
	if inf, ok := c.currentVMCache().informer(vmGVR, namespace); ok && !c.writes.recentlyWritten(vmGVR, namespace, name) {
		obj, err := inf.Lister().ByNamespace(namespace).Get(name)
		switch {
		case err == nil:
			if u, ok := obj.(*unstructured.Unstructured); ok {
				c.trackOperation("GetVM.cache", time.Since(start))
				return unstructuredToVM(u.DeepCopy())
			}
		case apierrors.IsNotFound(err):
			c.trackOperation("GetVM.cache", time.Since(start))
			return nil, fmt.Errorf("failed to get VM: %w", err)
		}
		// Any other lister error: fall through to the API server.
	}
	c.trackOperation("GetVM.direct", time.Since(start))
	return c.GetVM(namespace, name)
}

// getVMIForRead is GetVMI served from the cache under the same rules as
// getVMForRead. A synced cache's "not found" means the VM has no running
// instance, which is what callers use to report power state Off.
func (c *Client) getVMIForRead(namespace, name string) (*kubevirtv1.VirtualMachineInstance, error) {
	start := time.Now()
	if inf, ok := c.currentVMCache().informer(vmiGVR, namespace); ok && !c.writes.recentlyWritten(vmiGVR, namespace, name) {
		obj, err := inf.Lister().ByNamespace(namespace).Get(name)
		switch {
		case err == nil:
			if u, ok := obj.(*unstructured.Unstructured); ok {
				c.trackOperation("GetVMI.cache", time.Since(start))
				return unstructuredToVMI(u.DeepCopy())
			}
		case apierrors.IsNotFound(err):
			c.trackOperation("GetVMI.cache", time.Since(start))
			return nil, fmt.Errorf("failed to get VMI %s: %w", name, err)
		}
	}
	c.trackOperation("GetVMI.direct", time.Since(start))
	return c.GetVMI(namespace, name)
}

// listVMNamesFromCache returns the names of the VMs in namespace matching
// selector, or false when the cache is not synced for that namespace.
func (c *Client) listVMNamesFromCache(namespace string, selector labels.Selector) ([]string, bool) {
	inf, ok := c.currentVMCache().informer(vmGVR, namespace)
	if !ok {
		return nil, false
	}
	objs, err := inf.Lister().ByNamespace(namespace).List(selector)
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(objs))
	for _, obj := range objs {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			names = append(names, u.GetName())
		}
	}
	c.trackOperation("ListVMs.cache", 0)
	return names, true
}

// filterVMNames applies the selector's explicit name list, if any, and sorts
// the result so collections render in a stable order regardless of source.
func filterVMNames(names []string, vmSelector *VMSelectorConfig) []string {
	var out []string
	if vmSelector != nil && len(vmSelector.Names) > 0 {
		allowed := make(map[string]bool, len(vmSelector.Names))
		for _, n := range vmSelector.Names {
			allowed[n] = true
		}
		for _, n := range names {
			if allowed[n] {
				out = append(out, n)
			}
		}
	} else {
		out = append(out, names...)
	}
	sort.Strings(out)
	return out
}
