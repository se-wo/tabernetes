package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/apis/resource"
	_ "k8s.io/kubernetes/pkg/apis/resource/install"
	resourcevalidation "k8s.io/kubernetes/pkg/apis/resource/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

var testAdapter = &gpuAdapter{
	Vendor: "google", Architecture: "swiftshader", Fallback: true, JSPI: true,
	Description: strings.Repeat("a very long adapter description ", 4),
	Features:    []string{"shader-f16", "timestamp-query"},
	Limits: map[string]int64{
		"maxBufferSize": 1 << 30, "maxStorageBufferBindingSize": 128 << 20,
		"maxComputeInvocationsPerWorkgroup": 256, "maxComputeWorkgroupsPerDimension": 65535,
	},
}

// The slice passes the validation of the apiserver.
func TestWebGPUSliceValid(t *testing.T) {
	slice := webgpuSlice("tab-worker-1", "uid", testAdapter, "Chrome 142", 1)
	internal := &resource.ResourceSlice{}
	if err := legacyscheme.Scheme.Convert(slice, internal, nil); err != nil {
		t.Fatal(err)
	}
	if errs := resourcevalidation.ValidateResourceSlice(internal); len(errs) > 0 {
		t.Fatalf("invalid slice: %v", errs.ToAggregate())
	}
	d := slice.Spec.Devices[0]
	for name, want := range map[resourceapi.QualifiedName]any{
		"vendor": "google", "fallback": true, "shaderF16": true, "subgroups": false,
		"maxComputeInvocations": int64(256), "wasmJSPI": true,
	} {
		a, ok := d.Attributes[name]
		if !ok {
			t.Errorf("attribute %s is missing", name)
			continue
		}
		var got any
		switch {
		case a.StringValue != nil:
			got = *a.StringValue
		case a.BoolValue != nil:
			got = *a.BoolValue
		case a.IntValue != nil:
			got = *a.IntValue
		}
		if got != want {
			t.Errorf("attribute %s = %v, want %v", name, got, want)
		}
	}
	if !ptr.Deref(d.AllowMultipleAllocations, false) {
		t.Error("the adapter is not shareable")
	}
}

// The DeviceClass of the addons passes the validation of the apiserver.
func TestWebGPUDeviceClassValid(t *testing.T) {
	b, err := addonFS.ReadFile("addons/webgpu.yaml")
	if err != nil {
		t.Fatal(err)
	}
	class := &resourceapi.DeviceClass{}
	if err := yaml.UnmarshalStrict(b, class); err != nil {
		t.Fatal(err)
	}
	internal := &resource.DeviceClass{}
	if err := legacyscheme.Scheme.Convert(class, internal, nil); err != nil {
		t.Fatal(err)
	}
	if errs := resourcevalidation.ValidateDeviceClass(internal); len(errs) > 0 {
		t.Fatalf("invalid class: %v", errs.ToAggregate())
	}
	if class.Name != webgpuDriver {
		t.Errorf("class %s, want %s", class.Name, webgpuDriver)
	}
}

func allocated(name string, pod *v1.Pod, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pod.Namespace},
		Status: resourceapi.ResourceClaimStatus{
			Allocation:  &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: results}},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}},
		},
	}
}

func result(request, driver, pool string) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{Request: request, Driver: driver, Pool: pool, Device: webgpuDevice, ShareID: ptr.To(types.UID("share"))}
}

func TestClaimDevices(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pod-uid"},
		Spec: v1.PodSpec{
			ResourceClaims: []v1.PodResourceClaim{
				{Name: "gpu", ResourceClaimTemplateName: ptr.To("gpu-template")},
				{Name: "shared", ResourceClaimName: ptr.To("shared-gpu")},
			},
			InitContainers: []v1.Container{{Name: "init"}},
			Containers: []v1.Container{
				{Name: "a", Resources: v1.ResourceRequirements{Claims: []v1.ResourceClaim{{Name: "gpu"}}}},
				{Name: "b", Resources: v1.ResourceRequirements{Claims: []v1.ResourceClaim{{Name: "shared", Request: "one"}}}},
				{Name: "c"},
				{Name: "d", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{"tabernetes.io/webgpu": apiresource.MustParse("1")}}},
			},
		},
		Status: v1.PodStatus{
			ResourceClaimStatuses: []v1.PodResourceClaimStatus{{Name: "gpu", ResourceClaimName: ptr.To("p-gpu-x7k2")}},
			ExtendedResourceClaimStatus: &v1.PodExtendedResourceClaimStatus{
				ResourceClaimName: "p-extended-resources-q8",
				RequestMappings:   []v1.ContainerExtendedResourceRequest{{ContainerName: "d", ResourceName: "tabernetes.io/webgpu", RequestName: "container-3-request-0"}},
			},
		},
	}
	claims := map[string]*resourceapi.ResourceClaim{
		"p-gpu-x7k2":              allocated("p-gpu-x7k2", pod, result("gpu", webgpuDriver, "tab")),
		"shared-gpu":              allocated("shared-gpu", pod, result("one", webgpuDriver, "tab"), result("two/sub", webgpuDriver, "tab")),
		"p-extended-resources-q8": allocated("p-extended-resources-q8", pod, result("container-3-request-0", webgpuDriver, "tab")),
	}
	got, err := claimDevices(pod, claims, "tab")
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string][]string{}
	for ctr, devs := range got {
		for _, d := range devs {
			statuses[ctr] = append(statuses[ctr], string(d.status)+" "+d.claim+" "+d.request)
		}
	}
	want := map[string][]string{
		"a": {"claim:gpu p-gpu-x7k2 gpu"},
		"b": {"claim:shared/one shared-gpu one"},
		"d": {"claim:p-extended-resources-q8/container-3-request-0 p-extended-resources-q8 container-3-request-0"},
	}
	if !reflect.DeepEqual(statuses, want) {
		t.Errorf("devices by container:\n got %v\nwant %v", statuses, want)
	}
	if env := deviceEnv(got["a"]); !reflect.DeepEqual(env, []string{"TABERNETES_WEBGPU_DEVICE=webgpu.tabernetes.io/tab/gpu-0"}) {
		t.Errorf("env %v", env)
	}

	w := &podWorker{devices: got}
	health := w.devicesHealthLocked("b", false)
	if len(health) != 1 || health[0].Name != "claim:shared/one" || len(health[0].Resources) != 1 ||
		health[0].Resources[0].ResourceID != "webgpu.tabernetes.io/tab/gpu-0" || health[0].Resources[0].Health != v1.ResourceHealthStatusUnhealthy {
		t.Errorf("health %+v", health)
	}

	for name, tc := range map[string]struct {
		claim *resourceapi.ResourceClaim
		err   string
	}{
		"not allocated": {&resourceapi.ResourceClaim{}, "not allocated"},
		"not reserved":  {&resourceapi.ResourceClaim{Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{}}}, "not reserved"},
		"other driver":  {allocated("x", pod, result("gpu", "gpu.nvidia.com", "tab")), "gpu.nvidia.com is not registered"},
		"other node":    {allocated("x", pod, result("gpu", webgpuDriver, "tab-worker-1")), "is not on node tab"},
	} {
		_, err := claimDevices(pod, map[string]*resourceapi.ResourceClaim{"p-gpu-x7k2": tc.claim}, "tab")
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: error %v, want %q", name, err, tc.err)
		}
	}
}

// prepareDevices waits for claims that do not exist yet, refuses claims of
// another pod and does not prepare anything while the adapter is gone, also
// for a claim that no container lists.
func TestPrepareDevices(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pod-uid"},
		Spec: v1.PodSpec{
			ResourceClaims: []v1.PodResourceClaim{{Name: "gpu", ResourceClaimTemplateName: ptr.To("gpu-template")}},
			Containers:     []v1.Container{{Name: "a"}}, // the claim is pod-level only
		},
	}
	claim := allocated("p-gpu-1", pod, result("gpu", webgpuDriver, tabNodeName))
	claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "p", UID: "pod-uid", Controller: ptr.To(true)}}
	n := &tabNode{client: fake.NewClientset(claim), admin: fake.NewClientset(claim)}
	w := &podWorker{n: n, pod: pod}
	ctx := context.Background()

	if err := w.prepareDevices(ctx); err == nil || !strings.Contains(err.Error(), "not created yet") {
		t.Errorf("before the claim is in the pod status: %v", err)
	}

	pod.Status.ResourceClaimStatuses = []v1.PodResourceClaimStatus{{Name: "gpu", ResourceClaimName: ptr.To("p-gpu-1")}}
	if err := w.prepareDevices(ctx); err == nil || !strings.Contains(err.Error(), "adapter of this browser is gone") {
		t.Errorf("without the adapter: %v", err)
	}

	n.gpu = testAdapter
	if err := w.prepareDevices(ctx); err != nil {
		t.Fatalf("with the adapter: %v", err)
	}
	got, err := n.admin.ResourceV1().ResourceClaims("ns").Get(ctx, "p-gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Devices) != 1 || got.Status.Devices[0].Conditions[0].Type != webgpuReadyCond {
		t.Errorf("claim status %+v", got.Status.Devices)
	}

	other := pod.DeepCopy()
	other.UID = "another-pod"
	claim.Status.ReservedFor[0].UID = "another-pod"
	w2 := &podWorker{n: &tabNode{client: fake.NewClientset(claim), admin: fake.NewClientset(claim), gpu: testAdapter}, pod: other}
	if err := w2.prepareDevices(ctx); err == nil || !strings.Contains(err.Error(), "Pod is not owner") {
		t.Errorf("claim of another pod: %v", err)
	}
}
