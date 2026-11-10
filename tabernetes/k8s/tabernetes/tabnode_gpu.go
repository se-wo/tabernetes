package main

// WebGPU through Dynamic Resource Allocation (resource.k8s.io/v1). The
// kubelets in the browser, the tab node and the nodes in Web Workers started
// with "addnode --gpu", offer the WebGPU adapter of the browser as a device of
// the DRA driver webgpu.tabernetes.io:
//
//   - Detection: the page (web/src/gpu.js, globalThis.tabernetesGPU) probes
//     navigator.gpu.requestAdapter() in the tab or in the worker of the node.
//   - Publishing: one ResourceSlice per node, pool = node name, one device
//     gpu-0 with the adapter as attributes. A DRA kubelet plugin would send
//     it to the kubelet over gRPC; here the kubelet is the driver and writes
//     it itself, as system:node:<name> (the Node authorizer and
//     NodeRestriction allow a node its own slices). The DeviceClass
//     webgpu.tabernetes.io comes with the addons (addons/webgpu.yaml).
//   - Allocation: the DynamicResources plugin of the scheduler, nothing here.
//   - Preparation: before the containers of a pod start, the kubelet reads the
//     allocated claims of the pod (the kubelet's PrepareDynamicResources) and
//     hands the device to the containers that list the claim in
//     resources.claims: their WebAssembly worker gets the tabernetes_webgpu
//     imports (web/src/wasi.worker.js), every container gets
//     TABERNETES_WEBGPU_DEVICE. It reports the device in the claim status
//     (status.devices) and its health in the container statuses
//     (allocatedResourcesStatus).
//
// The browser shares the GPU between all tabs and workers anyway, so the
// device can be allocated several times (allowMultipleAllocations): each
// allocation takes one of 8 "shares" by default, a claim that wants the GPU
// for itself asks for all 8.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall/js"
	"time"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/utils/ptr"
)

const (
	webgpuDriver      = "webgpu.tabernetes.io"
	webgpuDevice      = "gpu-0"
	webgpuShares      = 8
	webgpuEnvDevice   = "TABERNETES_WEBGPU_DEVICE"
	webgpuReadyCond   = "Ready"
	webgpuSliceSuffix = "-webgpu" // the slice of node n is n-webgpu
)

// gpuOffered tells whether this node probes WebGPU and publishes the adapter:
// the tab node always, a node in a Web Worker with addnode --gpu.
var gpuOffered = true

// gpuAdapter is what gpu.js probe() reports.
type gpuAdapter struct {
	Vendor       string           `json:"vendor"`
	Architecture string           `json:"architecture"`
	Device       string           `json:"device"`
	Description  string           `json:"description"`
	Fallback     bool             `json:"fallback"`
	Features     []string         `json:"features"`
	Limits       map[string]int64 `json:"limits"`
	JSPI         bool             `json:"jspi"`
}

func (a *gpuAdapter) String() string {
	s := a.Description
	if s == "" {
		s = strings.TrimSpace(a.Vendor + " " + a.Architecture)
	}
	if s == "" {
		s = "WebGPU adapter"
	}
	if a.Fallback {
		s += " (software fallback)"
	}
	return s
}

// probeGPU asks the page for the adapter. nil without WebGPU.
func probeGPU(ctx context.Context) (*gpuAdapter, error) {
	gpu := js.Global().Get("tabernetesGPU")
	if gpu.Type() != js.TypeObject {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := await(ctx, gpu.Call("probe"))
	if err != nil {
		return nil, err
	}
	if v.Type() != js.TypeString || v.String() == "" {
		return nil, nil
	}
	a := &gpuAdapter{}
	if err := json.Unmarshal([]byte(v.String()), a); err != nil {
		return nil, err
	}
	return a, nil
}

// Device attributes from the limits of the adapter. The names are C
// identifiers of at most 32 characters.
var webgpuLimitAttrs = map[string]resourceapi.QualifiedName{
	"maxBufferSize":                     "maxBufferSize",
	"maxStorageBufferBindingSize":       "maxStorageBufferBindingSize",
	"maxComputeWorkgroupStorageSize":    "maxComputeWorkgroupStorageSize",
	"maxComputeInvocationsPerWorkgroup": "maxComputeInvocations",
	"maxComputeWorkgroupsPerDimension":  "maxComputeWorkgroupsPerDimension",
}

// Optional features of WebGPU that become boolean attributes.
var webgpuFeatureAttrs = map[string]resourceapi.QualifiedName{
	"shader-f16":      "shaderF16",
	"subgroups":       "subgroups",
	"timestamp-query": "timestampQuery",
}

func attrString(s string) resourceapi.DeviceAttribute {
	if len(s) > resourceapi.DeviceAttributeMaxValueLength {
		s = s[:resourceapi.DeviceAttributeMaxValueLength]
	}
	return resourceapi.DeviceAttribute{StringValue: &s}
}

// webgpuDeviceSpec describes the adapter as a DRA device.
func webgpuDeviceSpec(a *gpuAdapter, browser string) resourceapi.Device {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type":         attrString("webgpu"),
		"vendor":       attrString(a.Vendor),
		"architecture": attrString(a.Architecture),
		"browser":      attrString(browser),
		"fallback":     {BoolValue: ptr.To(a.Fallback)},
		// The tabernetes_webgpu imports of WebAssembly pods wait for the GPU
		// with JavaScript Promise Integration.
		"wasmJSPI": {BoolValue: ptr.To(a.JSPI)},
	}
	if a.Device != "" {
		attrs["device"] = attrString(a.Device)
	}
	if a.Description != "" {
		attrs["description"] = attrString(a.Description)
	}
	for f, name := range webgpuFeatureAttrs {
		attrs[name] = resourceapi.DeviceAttribute{BoolValue: ptr.To(slices.Contains(a.Features, f))}
	}
	for l, name := range webgpuLimitAttrs {
		if v, ok := a.Limits[l]; ok {
			attrs[name] = resourceapi.DeviceAttribute{IntValue: ptr.To(v)}
		}
	}
	return resourceapi.Device{
		Name:       webgpuDevice,
		Attributes: attrs,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"shares": {
				Value: *resource.NewQuantity(webgpuShares, resource.DecimalSI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: resource.NewQuantity(1, resource.DecimalSI),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min:  resource.NewQuantity(1, resource.DecimalSI),
						Max:  resource.NewQuantity(webgpuShares, resource.DecimalSI),
						Step: resource.NewQuantity(1, resource.DecimalSI),
					},
				},
			},
		},
		AllowMultipleAllocations: ptr.To(true),
	}
}

// webgpuSlice is the ResourceSlice of node with the adapter a.
func webgpuSlice(node string, nodeUID types.UID, a *gpuAdapter, browser string, generation int64) *resourceapi.ResourceSlice {
	return &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: node + webgpuSliceSuffix,
			// Like the slices of the kubelet: they go with the node.
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node, UID: nodeUID, Controller: ptr.To(true)}},
		},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   webgpuDriver,
			Pool:     resourceapi.ResourcePool{Name: node, Generation: generation, ResourceSliceCount: 1},
			NodeName: ptr.To(node),
			Devices:  []resourceapi.Device{webgpuDeviceSpec(a, browser)},
		},
	}
}

// syncGPU probes the adapter and publishes it, or removes the slice when the
// adapter is gone. It runs at the start and with every node status update.
// This node is the only writer of its slice, so it talks to the apiserver
// only when the device differs from what it published last; a node without
// WebGPU never does (the cluster starts fresh with the page, there is no old
// slice to remove).
func (n *tabNode) syncGPU(ctx context.Context) {
	if !gpuOffered {
		return
	}
	n.gpuSync.Lock()
	defer n.gpuSync.Unlock()
	a, err := probeGPU(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tab node: probing WebGPU:", err)
		return // keep what is published
	}
	n.gpuMu.Lock()
	n.gpu = a
	n.gpuMu.Unlock()
	rs := n.client.ResourceV1().ResourceSlices()
	name := tabNodeName + webgpuSliceSuffix
	if a == nil {
		if n.gpuPublished == nil {
			return
		}
		fmt.Fprintln(os.Stderr, "tab node: the WebGPU adapter is gone")
		err := rs.Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !k8serrors.IsNotFound(err) {
			fmt.Fprintln(os.Stderr, "tab node: deleting the WebGPU slice:", err)
			return // try again next time
		}
		n.gpuPublished = nil
		return
	}
	ua, _, _ := browserInfo()
	want := webgpuSlice(tabNodeName, n.nodeUID, a, ua, 1)
	dev := &want.Spec.Devices[0]
	if n.gpuPublished != nil && equality.Semantic.DeepEqual(n.gpuPublished, dev) {
		return
	}
	if n.gpuPublished == nil {
		_, err = rs.Create(ctx, want, metav1.CreateOptions{})
		if err == nil {
			n.gpuPublished = dev
			fmt.Fprintf(os.Stderr, "tab node: offers %s as %s/%s/%s\n", a, webgpuDriver, tabNodeName, webgpuDevice)
			return
		}
		if !k8serrors.IsAlreadyExists(err) {
			fmt.Fprintln(os.Stderr, "tab node: publishing the WebGPU slice:", err)
			return // try again next time
		}
	}
	old, err := rs.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tab node: reading the WebGPU slice:", err)
		return
	}
	// A new generation tells the scheduler that the pool changed.
	want.Spec.Pool.Generation = old.Spec.Pool.Generation + 1
	old.Spec = want.Spec
	if _, err := rs.Update(ctx, old, metav1.UpdateOptions{}); err != nil {
		fmt.Fprintln(os.Stderr, "tab node: updating the WebGPU slice:", err)
		return
	}
	n.gpuPublished = dev
}

func (n *tabNode) gpuAdapter() *gpuAdapter {
	n.gpuMu.Lock()
	defer n.gpuMu.Unlock()
	return n.gpu
}

// preparedDevice is a device of this node that a container may use.
type preparedDevice struct {
	claim   string          // the ResourceClaim
	request string          // the request of the claim that got the device
	status  v1.ResourceName // the name in allocatedResourcesStatus
	result  resourceapi.DeviceRequestAllocationResult
}

func (d preparedDevice) id() string {
	return d.result.Driver + "/" + d.result.Pool + "/" + d.result.Device
}

// podHasClaims tells whether the pod needs PrepareDynamicResources.
func podHasClaims(pod *v1.Pod) bool {
	return len(pod.Spec.ResourceClaims) > 0 || pod.Status.ExtendedResourceClaimStatus != nil
}

// claimDevices works out which container of the pod gets which device, from
// the allocated claims of the pod (by name). It fails when a claim is not
// allocated or reserved for the pod yet, or when it needs a device that this
// node cannot prepare: one of another driver, or of another node's pool.
func claimDevices(pod *v1.Pod, claims map[string]*resourceapi.ResourceClaim, node string) (map[string][]preparedDevice, error) {
	for name, claim := range claims {
		if claim.Status.Allocation == nil {
			return nil, fmt.Errorf("ResourceClaim %s is not allocated yet", name)
		}
		if !resourceclaim.IsReservedForPod(pod, claim, false) {
			return nil, fmt.Errorf("ResourceClaim %s is not reserved for the pod", name)
		}
		for _, r := range claim.Status.Allocation.Devices.Results {
			if r.Driver != webgpuDriver {
				return nil, fmt.Errorf("ResourceClaim %s: DRA driver %s is not registered on node %s", name, r.Driver, node)
			}
			if r.Pool != node {
				return nil, fmt.Errorf("ResourceClaim %s: device %s/%s/%s is not on node %s", name, r.Driver, r.Pool, r.Device, node)
			}
		}
	}

	out := map[string][]preparedDevice{}
	add := func(ctr, claimName, request string, status v1.ResourceName) {
		claim := claims[claimName]
		if claim == nil {
			return
		}
		for _, r := range claim.Status.Allocation.Devices.Results {
			if request == "" || resourceclaim.BaseRequestRef(r.Request) == request {
				out[ctr] = append(out[ctr], preparedDevice{claim: claimName, request: r.Request, status: status, result: r})
			}
		}
	}
	// The real names of the claims in spec.resourceClaims.
	names := map[string]string{}
	for i := range pod.Spec.ResourceClaims {
		pc := &pod.Spec.ResourceClaims[i]
		if name, _, err := resourceclaim.Name(pod, pc); err == nil && name != nil {
			names[pc.Name] = *name
		}
	}
	all := append(append([]v1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	for _, c := range all {
		for _, rc := range c.Resources.Claims {
			status := v1.ResourceName("claim:" + rc.Name)
			if rc.Request != "" {
				status += v1.ResourceName("/" + rc.Request)
			}
			add(c.Name, names[rc.Name], rc.Request, status)
		}
	}
	// Extended resources backed by a DeviceClass (extendedResourceName): the
	// scheduler made one claim for the pod and maps containers to requests.
	if ext := pod.Status.ExtendedResourceClaimStatus; ext != nil {
		for _, m := range ext.RequestMappings {
			add(m.ContainerName, ext.ResourceClaimName, m.RequestName,
				v1.ResourceName("claim:"+ext.ResourceClaimName+"/"+m.RequestName))
		}
	}
	return out, nil
}

// prepareDevices is PrepareDynamicResources of the kubelet for the pod of
// w. Errors are retried by the caller.
func (w *podWorker) prepareDevices(ctx context.Context) error {
	pod, _ := w.podInfo()
	// A claim of a template exists once the resourceclaim controller wrote
	// its name into the pod status. Until then, wait like the kubelet does
	// (resourceclaim.PodClaims would skip it).
	for i := range pod.Spec.ResourceClaims {
		if _, _, err := resourceclaim.Name(pod, &pod.Spec.ResourceClaims[i]); err != nil {
			return fmt.Errorf("claim %s of the pod: %w", pod.Spec.ResourceClaims[i].Name, err)
		}
	}
	claims := map[string]*resourceapi.ResourceClaim{}
	for name, mustCheckOwner := range resourceclaim.PodClaims(pod) {
		claim, err := w.n.client.ResourceV1().ResourceClaims(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading ResourceClaim %s: %w", name, err)
		}
		// A claim made for the pod must belong to it, not to an older pod
		// of the same name.
		if mustCheckOwner {
			if err := resourceclaim.IsForPod(pod, claim, false); err != nil {
				return err
			}
		}
		claims[name] = claim
	}
	devs, err := claimDevices(pod, claims, tabNodeName)
	if err != nil {
		return err
	}
	a := w.n.gpuAdapter()
	for _, claim := range claims {
		// claimDevices made sure every device of the claims is ours, also
		// those of claims that no container lists.
		if a == nil && len(claim.Status.Allocation.Devices.Results) > 0 {
			return fmt.Errorf("%s/%s/%s: the WebGPU adapter of this browser is gone", webgpuDriver, tabNodeName, webgpuDevice)
		}
	}
	for name, claim := range claims {
		w.n.reportClaimDevices(ctx, claim, a, name)
	}
	w.mu.Lock()
	w.devices = devs
	w.mu.Unlock()
	return nil
}

// reportClaimDevices writes the prepared devices of this node into
// status.devices of the claim, with the adapter as data. The node itself may
// not update claims; a DRA driver does it with a service account of its own,
// here the admin credentials of the tab stand in for it.
func (n *tabNode) reportClaimDevices(ctx context.Context, claim *resourceapi.ResourceClaim, a *gpuAdapter, name string) {
	data, _ := json.Marshal(a)
	for attempt := 0; attempt < 3; attempt++ {
		changed := false
		for _, r := range claim.Status.Allocation.Devices.Results {
			if r.Driver != webgpuDriver || r.Pool != tabNodeName {
				continue
			}
			if slices.ContainsFunc(claim.Status.Devices, func(s resourceapi.AllocatedDeviceStatus) bool {
				return s.Driver == r.Driver && s.Pool == r.Pool && s.Device == r.Device && ptr.Deref(s.ShareID, "") == string(ptr.Deref(r.ShareID, ""))
			}) {
				continue
			}
			st := resourceapi.AllocatedDeviceStatus{
				Driver: r.Driver, Pool: r.Pool, Device: r.Device,
				Conditions: []metav1.Condition{{
					Type: webgpuReadyCond, Status: metav1.ConditionTrue, Reason: "Prepared",
					Message:            fmt.Sprintf("%s, prepared for the WebAssembly workers of node %s", a, tabNodeName),
					LastTransitionTime: metav1.Now(), ObservedGeneration: claim.Generation,
				}},
				Data: &runtime.RawExtension{Raw: data},
			}
			if r.ShareID != nil {
				st.ShareID = ptr.To(string(*r.ShareID))
			}
			claim.Status.Devices = append(claim.Status.Devices, st)
			changed = true
		}
		if !changed {
			return
		}
		_, err := n.admin.ResourceV1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{})
		if err == nil || !k8serrors.IsConflict(err) {
			if err != nil {
				fmt.Fprintf(os.Stderr, "tab node: status of ResourceClaim %s/%s: %v\n", claim.Namespace, name, err)
			}
			return
		}
		latest, err := n.admin.ResourceV1().ResourceClaims(claim.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || latest.Status.Allocation == nil {
			return
		}
		claim = latest
	}
}

// deviceEnv is the environment for the devices of a container, like a CDI
// spec would add it.
func deviceEnv(devs []preparedDevice) []string {
	if len(devs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(devs))
	for _, d := range devs {
		if !slices.Contains(ids, d.id()) {
			ids = append(ids, d.id())
		}
	}
	return []string{webgpuEnvDevice + "=" + strings.Join(ids, ",")}
}

// containerDevices are the prepared devices of a container.
func (w *podWorker) containerDevices(name string) []preparedDevice {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.devices[name]
}

// gpuForWorker is the gpu option of the WebAssembly worker of a container
// (web/src/wasi.worker.js): the device and the adapter, or null.
func (w *podWorker) gpuForWorker(name string) js.Value {
	devs := w.containerDevices(name)
	a := w.n.gpuAdapter()
	if len(devs) == 0 || a == nil {
		return js.Null()
	}
	d := devs[0]
	b, _ := json.Marshal(map[string]any{
		"driver": d.result.Driver, "pool": d.result.Pool, "device": d.result.Device,
		"shareID": ptr.Deref(d.result.ShareID, ""), "claim": d.claim, "request": d.request,
		"adapter": a,
	})
	return js.Global().Get("JSON").Call("parse", string(b))
}

// devicesHealthLocked is allocatedResourcesStatus for the devices of a container:
// healthy while the adapter is there. w.mu is held.
func (w *podWorker) devicesHealthLocked(name string, healthy bool) []v1.ResourceStatus {
	var out []v1.ResourceStatus
	health := v1.ResourceHealthStatusHealthy
	if !healthy {
		health = v1.ResourceHealthStatusUnhealthy
	}
	for _, d := range w.devices[name] {
		i := slices.IndexFunc(out, func(s v1.ResourceStatus) bool { return s.Name == d.status })
		if i < 0 {
			out = append(out, v1.ResourceStatus{Name: d.status})
			i = len(out) - 1
		}
		id := v1.ResourceID(d.id())
		if !slices.ContainsFunc(out[i].Resources, func(r v1.ResourceHealth) bool { return r.ResourceID == id }) {
			out[i].Resources = append(out[i].Resources, v1.ResourceHealth{ResourceID: id, Health: health})
		}
	}
	return out
}
