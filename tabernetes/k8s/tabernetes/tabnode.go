package main

// The tab node: a kubelet inside the browser tab. It registers the Node "tab",
// keeps its Lease fresh and runs the pods the scheduler binds to it. A
// container is not a process here. It is a goroutine that runs a shell
// (mvdan.cc/sh) or one of the built-in images (tabnode_images.go) in the
// in-process network of the wasm, or a WebAssembly module in a Web Worker
// (tabnode_wasi.go). The apiserver reaches the node on 10.0.0.1:10250 for
// logs, exec, attach and port-forward (tabnode_server.go), and the pods on
// their pod IPs, so even the service proxy of the apiserver works.
//
// The node has the taint tabernetes.io/tab=true:PreferNoSchedule: as long as
// it is the only node, everything lands here. Once a Docker node joins, the
// scheduler prefers that one. The node "tab" next to the control plane is
// also the control plane node, like the first node of kubeadm: it has the
// role label node-role.kubernetes.io/control-plane, a second soft taint
// node-role.kubernetes.io/control-plane:PreferNoSchedule (so the scheduler
// prefers a node in a Web Worker over it), and the static pods of
// kube-apiserver, kube-controller-manager, kube-scheduler and etcd as
// mirror pods (tabnode_static.go). The node info names Tabernetes as the
// OS, not the browser.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall/js"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/version"
	"k8s.io/utils/ptr"
	npaclient "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
)

// The node of this wasm instance: "tab" next to the control plane, or a
// node in a Web Worker (nodeworker.go). One node per instance.
var (
	tabNodeName = "tab"
	tabNodeIP   = advertiseIP
)

const (
	tabTaintKey  = "tabernetes.io/tab"
	tabPodsDir   = "/var/lib/tabnode/pods"
	kubeletPort  = 10250
	leaseSeconds = 40

	// kernelVersion is the kernel of every tab node: the node info and
	// uname -r in its containers (tabnode_proc.go) say the same.
	kernelVersion = "6.0.0-tabernetes"

	controlPlaneRole = "node-role.kubernetes.io/control-plane"
)

type tabNode struct {
	client   kubernetes.Interface // as system:node:tab
	admin    kubernetes.Interface // for the service proxy (tabnode_net.go)
	recorder record.EventRecorder
	nodeUID  types.UID
	caPEM    []byte

	ipMu    sync.Mutex
	podCIDR *net.IPNet
	usedIPs map[string]bool

	mu      sync.Mutex
	workers map[types.UID]*podWorker

	net    *serviceProxy
	policy *netPolicy // NetworkPolicy (tabnode_netpol.go)

	gpuMu        sync.Mutex
	gpu          *gpuAdapter         // the WebGPU adapter it offers (tabnode_gpu.go), nil for none
	gpuSync      sync.Mutex          // one syncGPU at a time
	gpuPublished *resourceapi.Device // the device in the slice, nil without a slice; under gpuSync

	static *staticPods // the control plane pods, nil but on the control plane node (tabnode_static.go)
}

var theTabNode *tabNode

// nodeCreds are what a tab node needs to talk to the apiserver: its client
// certificate as system:node:<name>, the serving certificate of its kubelet
// API and the admin token for the service proxy. PEM encoded.
type nodeCreds struct {
	ca, cert, key, servingCert, servingKey []byte
	adminToken                             string
}

// issueNodeCreds issues the credentials of the node name with IP ip. The
// kubelet authenticates as system:node:<name>, so the Node authorizer and
// NodeRestriction (--enable-admission-plugins in main.go) treat it like any
// other kubelet: it may only write its own Node, Lease and ResourceSlices,
// the status of its own pods, and tokens bound to them.
func (c *cluster) issueNodeCreds(name, ip string) (nodeCreds, error) {
	c.ca.issue("node-"+name, "system:node:"+name, []string{"system:nodes"}, false, nil, nil)
	c.ca.issue("node-"+name+"-serving", name, nil, true, []string{name}, []net.IP{net.ParseIP(ip)})
	nc := nodeCreds{ca: c.ca.caPEM, adminToken: c.adminToken}
	for _, f := range []struct {
		dst  *[]byte
		file string
	}{
		{&nc.cert, "node-" + name + ".crt"}, {&nc.key, "node-" + name + ".key"},
		{&nc.servingCert, "node-" + name + "-serving.crt"}, {&nc.servingKey, "node-" + name + "-serving.key"},
	} {
		b, err := os.ReadFile(p(f.file))
		if err != nil {
			return nc, err
		}
		*f.dst = b
		os.Remove(p(f.file)) // the keys of another node stay out of this one
	}
	return nc, nil
}

// startTabNode registers the node and runs it until the tab closes.
func startTabNode(ctx context.Context, nc nodeCreds) error {
	// servingTLS reads the serving certificate from the file system.
	if err := os.WriteFile(p("tab-kubelet-serving.crt"), nc.servingCert, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(p("tab-kubelet-serving.key"), nc.servingKey, 0o600); err != nil {
		return err
	}
	cfg := &rest.Config{
		Host:            "https://127.0.0.1:6443",
		TLSClientConfig: rest.TLSClientConfig{CAData: nc.ca, CertData: nc.cert, KeyData: nc.key},
		QPS:             50,
		Burst:           100,
		UserAgent:       "tabernetes-kubelet/" + version.Get().GitVersion,
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	adminCfg := &rest.Config{
		Host:            "https://127.0.0.1:6443",
		BearerToken:     nc.adminToken,
		TLSClientConfig: rest.TLSClientConfig{CAData: nc.ca},
		QPS:             50,
		Burst:           100,
		UserAgent:       "tabernetes-proxy/" + version.Get().GitVersion,
	}
	admin, err := kubernetes.NewForConfig(adminCfg)
	if err != nil {
		return err
	}

	n := &tabNode{client: client, admin: admin, caPEM: nc.ca, usedIPs: map[string]bool{}, workers: map[types.UID]*podWorker{}}
	if comps := controlPlaneComponents(); len(comps) > 0 {
		n.static = newStaticPods(n, comps)
	}
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: client.CoreV1().Events("")})
	n.recorder = broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "kubelet", Host: tabNodeName})
	theTabNode = n

	if err := n.register(ctx); err != nil {
		return fmt.Errorf("registering node %s: %w", tabNodeName, err)
	}
	if err := n.serve(ctx); err != nil {
		return fmt.Errorf("kubelet server: %w", err)
	}
	go n.heartbeat(ctx)
	if n.static != nil {
		if err := n.static.start(ctx); err != nil {
			return fmt.Errorf("control plane pods: %w", err)
		}
	}
	go n.sampleStats(ctx)
	// The WebGPU adapter of the browser as a DRA device (tabnode_gpu.go).
	// Nothing of the node waits for the probe.
	go n.syncGPU(ctx)

	n.net = newServiceProxy(admin)
	go n.net.run(ctx)
	go n.handoverCoreDNS(ctx) // tabnode_coredns.go
	// CRDs do not speak protobuf: a client of its own for them.
	npa, err := npaclient.NewForConfig(adminCfg)
	if err != nil {
		return err
	}
	if n.policy, err = newNetPolicy(ctx, admin, npa, tabNodeName); err != nil {
		return fmt.Errorf("network policy: %w", err)
	}

	// The pod CIDR comes from the node IPAM controller of the
	// controller-manager, a moment after the node exists.
	if err := n.waitPodCIDR(ctx); err != nil {
		return err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", tabNodeName).String()
		}))
	podInformer := factory.Core().V1().Pods().Informer()
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { n.podChanged(obj.(*v1.Pod)) },
		UpdateFunc: func(_, obj any) { n.podChanged(obj.(*v1.Pod)) },
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			if pod, ok := obj.(*v1.Pod); ok {
				if n.static.observeGone(pod) { // a mirror pod
					return
				}
				n.podGone(pod.UID)
			}
		},
	})
	factory.Start(ctx.Done())
	return nil
}

func (n *tabNode) nodeLabels() map[string]string {
	return map[string]string{
		"kubernetes.io/hostname": tabNodeName,
		"kubernetes.io/os":       "linux",
		"kubernetes.io/arch":     "wasm",
		tabTaintKey:              "true",
		"tabernetes.io/wasm":     "true", // WebAssembly pods (runtimeclass.yaml)
		podLinkNodeLabel:         "v1",   // pods of other nodes reach it (podlink.go)
	}
}

// register creates the Node and writes its first status. A failure may be
// transient (the apiserver is busy while several nodes start), and a node
// that gives up stays NotReady for good, so it tries for two minutes.
func (n *tabNode) register(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	for wait := 500 * time.Millisecond; ; wait = min(2*wait, 5*time.Second) {
		err := n.registerOnce(ctx)
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			return err
		}
		fmt.Fprintf(os.Stderr, "tab node: registering node %s: %v; trying again\n", tabNodeName, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
}

func (n *tabNode) registerOnce(ctx context.Context) error {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: tabNodeName, Labels: n.nodeLabels()},
		Spec:       v1.NodeSpec{Taints: tabNodeTaints(n.static != nil)},
	}
	created, err := n.client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = n.client.CoreV1().Nodes().Get(ctx, tabNodeName, metav1.GetOptions{})
	}
	if err != nil {
		return err
	}
	n.nodeUID = created.UID
	return n.updateStatus(ctx)
}

// tabNodeTaints are the taints of a new tab node. Both are soft: alone, the
// control plane node takes every pod. With two of them, the scheduler
// prefers a node in a Web Worker (one) over the control plane node, and a
// Docker node (none) over both.
func tabNodeTaints(controlPlane bool) []v1.Taint {
	taints := []v1.Taint{{Key: tabTaintKey, Value: "true", Effect: v1.TaintEffectPreferNoSchedule}}
	if controlPlane {
		taints = append([]v1.Taint{{Key: controlPlaneRole, Effect: v1.TaintEffectPreferNoSchedule}}, taints...)
	}
	return taints
}

// The identity of this node while it runs: a machine ID of 32 hex
// characters like /etc/machine-id, and a system UUID and boot ID like the
// ones of a real machine.
var (
	machineID  = strings.ReplaceAll(string(uuid.NewUUID()), "-", "")
	systemUUID = string(uuid.NewUUID())
	bootID     = string(uuid.NewUUID())
)

// tabNodeInfo is the system info of a tab node. It names Tabernetes as the
// OS, like a distribution such as Talos, not the browser.
func tabNodeInfo() v1.NodeSystemInfo {
	return v1.NodeSystemInfo{
		MachineID:               machineID,
		SystemUUID:              systemUUID,
		BootID:                  bootID,
		KernelVersion:           kernelVersion,
		OSImage:                 "Tabernetes " + version.Get().GitVersion,
		ContainerRuntimeVersion: "tabernetes://" + version.Get().GitVersion,
		KubeletVersion:          version.Get().GitVersion,
		OperatingSystem:         "linux",
		Architecture:            "wasm",
	}
}

// browserInfo describes the browser: "Chrome 131" or so (the WebGPU device
// of tabnode_gpu.go names it), and its CPUs and memory for the capacity.
func browserInfo() (ua string, cpus int, memGiB int) {
	cpus, memGiB = 1, 4
	nav := js.Global().Get("navigator")
	if !nav.Truthy() {
		return "unknown browser", cpus, memGiB
	}
	ua = nav.Get("userAgent").String()
	for _, b := range []string{"Edg/", "Firefox/", "Chrome/", "Version/"} {
		if i := strings.Index(ua, b); i >= 0 {
			name := strings.TrimSuffix(b, "/")
			if name == "Version" {
				name = "Safari"
			}
			v := ua[i+len(b):]
			v, _, _ = strings.Cut(v, " ")
			v, _, _ = strings.Cut(v, ".")
			ua = name + " " + v
			break
		}
	}
	if hc := nav.Get("hardwareConcurrency"); hc.Type() == js.TypeNumber && hc.Int() > 0 {
		cpus = hc.Int()
	}
	if dm := nav.Get("deviceMemory"); dm.Type() == js.TypeNumber && dm.Float() >= 1 {
		memGiB = int(dm.Float())
	}
	return ua, cpus, memGiB
}

// updateStatus writes the node status as a patch of the status fields the
// kubelet owns, like the kubelet does. An update of the whole object would
// also carry the spec as it was read: when the node lifecycle controller
// adds a taint in between, NodeRestriction sees the node remove that taint
// and refuses the write ("not allowed to modify taints").
func (n *tabNode) updateStatus(ctx context.Context) error {
	node, err := n.client.CoreV1().Nodes().Get(ctx, tabNodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"status": n.nodeStatus(node.Status.Conditions)})
	if err != nil {
		return err
	}
	_, err = n.client.CoreV1().Nodes().Patch(ctx, tabNodeName, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "status")
	return err
}

// nodeStatus is the status this kubelet reports. old are the conditions of
// the node as it is, for their transition times.
func (n *tabNode) nodeStatus(old []v1.NodeCondition) v1.NodeStatus {
	_, cpus, mem := browserInfo()
	now := metav1.Now()
	capacity := v1.ResourceList{
		v1.ResourceCPU:              *resource.NewQuantity(int64(cpus), resource.DecimalSI),
		v1.ResourceMemory:           *resource.NewQuantity(int64(mem)<<30, resource.BinarySI),
		v1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
		v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
	cond := func(t v1.NodeConditionType, s v1.ConditionStatus, reason, msg string) v1.NodeCondition {
		c := v1.NodeCondition{Type: t, Status: s, Reason: reason, Message: msg, LastHeartbeatTime: now, LastTransitionTime: now}
		for _, o := range old {
			if o.Type == t && o.Status == s {
				c.LastTransitionTime = o.LastTransitionTime
			}
		}
		return c
	}
	return v1.NodeStatus{
		Capacity:    capacity,
		Allocatable: capacity,
		Conditions: []v1.NodeCondition{
			cond(v1.NodeMemoryPressure, v1.ConditionFalse, "KubeletHasSufficientMemory", "kubelet has sufficient memory available"),
			cond(v1.NodeDiskPressure, v1.ConditionFalse, "KubeletHasNoDiskPressure", "kubelet has no disk pressure"),
			cond(v1.NodePIDPressure, v1.ConditionFalse, "KubeletHasSufficientPID", "kubelet has sufficient PID available"),
			cond(v1.NodeReady, v1.ConditionTrue, "KubeletReady", "kubelet is posting ready status"),
		},
		Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: tabNodeIP},
			{Type: v1.NodeHostName, Address: tabNodeName},
		},
		DaemonEndpoints: v1.NodeDaemonEndpoints{KubeletEndpoint: v1.DaemonEndpoint{Port: kubeletPort}},
		NodeInfo:        tabNodeInfo(),
		Images:          append(builtinImageList(), n.static.images()...),
	}
}

// heartbeat renews the node lease every 10 seconds and the node status every
// minute, like a kubelet.
func (n *tabNode) heartbeat(ctx context.Context) {
	leases := n.client.CoordinationV1().Leases(v1.NamespaceNodeLease)
	lastStatus := time.Now()
	for {
		now := metav1.NewMicroTime(time.Now())
		lease, err := leases.Get(ctx, tabNodeName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = leases.Create(ctx, &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: tabNodeName, OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Node", Name: tabNodeName, UID: n.nodeUID,
				}}},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       ptr.To(tabNodeName),
					LeaseDurationSeconds: ptr.To[int32](leaseSeconds),
					RenewTime:            &now,
				},
			}, metav1.CreateOptions{})
		case err == nil:
			lease.Spec.RenewTime = &now
			_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
		}
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "tab node: lease:", err)
		}
		if time.Since(lastStatus) > time.Minute {
			if err := n.updateStatus(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "tab node: status:", err)
			}
			n.syncGPU(ctx)
			lastStatus = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (n *tabNode) waitPodCIDR(ctx context.Context) error {
	for {
		node, err := n.client.CoreV1().Nodes().Get(ctx, tabNodeName, metav1.GetOptions{})
		if err == nil && node.Spec.PodCIDR != "" {
			_, cidr, err := net.ParseCIDR(node.Spec.PodCIDR)
			if err != nil {
				return err
			}
			n.ipMu.Lock()
			n.podCIDR = cidr
			n.ipMu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// allocIP hands out the next free address of the pod CIDR, .2 and up (.1 is
// the gateway on a real node).
func (n *tabNode) allocIP() (string, error) {
	n.ipMu.Lock()
	defer n.ipMu.Unlock()
	if n.podCIDR == nil {
		return "", errors.New("no pod CIDR yet")
	}
	base := binary.BigEndian.Uint32(n.podCIDR.IP.To4())
	ones, bits := n.podCIDR.Mask.Size()
	size := uint32(1) << (bits - ones)
	for i := uint32(2); i < size-1; i++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, base+i)
		if s := ip.String(); !n.usedIPs[s] {
			n.usedIPs[s] = true
			return s, nil
		}
	}
	return "", errors.New("pod CIDR " + n.podCIDR.String() + " is full")
}

func (n *tabNode) releaseIP(ip string) {
	n.ipMu.Lock()
	delete(n.usedIPs, ip)
	n.ipMu.Unlock()
}

// isTabPodIP reports whether ip belongs to a pod of this node.
func (n *tabNode) isTabPodIP(ip string) bool {
	n.ipMu.Lock()
	defer n.ipMu.Unlock()
	parsed := net.ParseIP(ip)
	return n.podCIDR != nil && parsed != nil && n.podCIDR.Contains(parsed)
}

func (n *tabNode) podChanged(pod *v1.Pod) {
	if n.static.observe(pod) { // a mirror pod
		return
	}
	n.mu.Lock()
	w := n.workers[pod.UID]
	if w == nil {
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			// Finished before this kubelet saw it (a reload never gets here,
			// the cluster dies with the tab). Only the deletion is left.
			n.mu.Unlock()
			if pod.DeletionTimestamp != nil {
				n.deletePod(pod)
			}
			return
		}
		w = newPodWorker(n, pod)
		n.workers[pod.UID] = w
		n.mu.Unlock()
		go w.run()
		return
	}
	n.mu.Unlock()
	w.update(pod)
}

func (n *tabNode) podGone(uid types.UID) {
	n.mu.Lock()
	w := n.workers[uid]
	delete(n.workers, uid)
	n.mu.Unlock()
	if w != nil {
		w.gone()
	}
}

// worker finds the pod worker of namespace/name.
func (n *tabNode) worker(namespace, name string) *podWorker {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, w := range n.workers {
		if w.namespace == namespace && w.name == name {
			return w
		}
	}
	return nil
}

func (n *tabNode) workerList() []*podWorker {
	n.mu.Lock()
	defer n.mu.Unlock()
	list := make([]*podWorker, 0, len(n.workers))
	for _, w := range n.workers {
		list = append(list, w)
	}
	return list
}

// deletePod finishes a graceful deletion: the containers are stopped, the
// pod object goes away.
func (n *tabNode) deletePod(pod *v1.Pod) {
	err := n.client.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To[int64](0),
		Preconditions:      &metav1.Preconditions{UID: &pod.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		fmt.Fprintf(os.Stderr, "tab node: deleting pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
	}
}

// servingTLS is the certificate of the kubelet API on 10.0.0.1:10250.
func servingTLS(caPEM []byte) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(p("tab-kubelet-serving.crt"), p("tab-kubelet-serving.key"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		NextProtos:   []string{"http/1.1"}, // SPDY upgrades need HTTP/1.1
	}, nil
}
