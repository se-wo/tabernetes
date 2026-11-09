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
// scheduler prefers that one.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall/js"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/version"
	"k8s.io/utils/ptr"
)

const (
	tabNodeName  = "tab"
	tabNodeIP    = advertiseIP
	tabTaintKey  = "tabernetes.io/tab"
	tabPodsDir   = "/var/lib/tabnode/pods"
	kubeletPort  = 10250
	leaseSeconds = 40
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

	net *serviceProxy
}

var theTabNode *tabNode

// startTabNode registers the node and runs it until the tab closes.
func startTabNode(ctx context.Context, c *cluster) error {
	// The kubelet authenticates as system:node:tab, so the Node authorizer
	// and NodeRestriction treat it like any other kubelet.
	c.ca.issue("tab-kubelet", "system:node:"+tabNodeName, []string{"system:nodes"}, false, nil, nil)
	c.ca.issue("tab-kubelet-serving", tabNodeName, nil, true, []string{tabNodeName}, []net.IP{net.ParseIP(tabNodeIP)})
	certPEM, err := os.ReadFile(p("tab-kubelet.crt"))
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(p("tab-kubelet.key"))
	if err != nil {
		return err
	}
	cfg := &rest.Config{
		Host:            "https://127.0.0.1:6443",
		TLSClientConfig: rest.TLSClientConfig{CAData: c.ca.caPEM, CertData: certPEM, KeyData: keyPEM},
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
		BearerToken:     c.adminToken,
		TLSClientConfig: rest.TLSClientConfig{CAData: c.ca.caPEM},
		QPS:             50,
		Burst:           100,
		UserAgent:       "tabernetes-proxy/" + version.Get().GitVersion,
	}
	admin, err := kubernetes.NewForConfig(adminCfg)
	if err != nil {
		return err
	}

	n := &tabNode{client: client, admin: admin, caPEM: c.ca.caPEM, usedIPs: map[string]bool{}, workers: map[types.UID]*podWorker{}}
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

	n.net = newServiceProxy(admin)
	go n.net.run(ctx)

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
				n.podGone(pod.UID)
			}
		},
	})
	factory.Start(ctx.Done())
	return nil
}

func (n *tabNode) nodeLabels() map[string]string {
	return map[string]string{
		"kubernetes.io/hostname":           tabNodeName,
		"kubernetes.io/os":                 "linux",
		"kubernetes.io/arch":               "wasm",
		"node.kubernetes.io/instance-type": "browser-tab",
		tabTaintKey:                        "true",
	}
}

func (n *tabNode) register(ctx context.Context) error {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: tabNodeName, Labels: n.nodeLabels()},
		Spec: v1.NodeSpec{Taints: []v1.Taint{{
			Key: tabTaintKey, Value: "true", Effect: v1.TaintEffectPreferNoSchedule,
		}}},
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

// browserInfo describes the browser for the node info: "Chrome/131" or so.
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

func (n *tabNode) updateStatus(ctx context.Context) error {
	node, err := n.client.CoreV1().Nodes().Get(ctx, tabNodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ua, cpus, mem := browserInfo()
	now := metav1.Now()
	capacity := v1.ResourceList{
		v1.ResourceCPU:              *resource.NewQuantity(int64(cpus), resource.DecimalSI),
		v1.ResourceMemory:           *resource.NewQuantity(int64(mem)<<30, resource.BinarySI),
		v1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
		v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
	cond := func(t v1.NodeConditionType, s v1.ConditionStatus, reason, msg string) v1.NodeCondition {
		c := v1.NodeCondition{Type: t, Status: s, Reason: reason, Message: msg, LastHeartbeatTime: now, LastTransitionTime: now}
		for _, old := range node.Status.Conditions {
			if old.Type == t && old.Status == s {
				c.LastTransitionTime = old.LastTransitionTime
			}
		}
		return c
	}
	node.Status.Capacity = capacity
	node.Status.Allocatable = capacity
	node.Status.Conditions = []v1.NodeCondition{
		cond(v1.NodeMemoryPressure, v1.ConditionFalse, "KubeletHasSufficientMemory", "kubelet has sufficient memory available"),
		cond(v1.NodeDiskPressure, v1.ConditionFalse, "KubeletHasNoDiskPressure", "kubelet has no disk pressure"),
		cond(v1.NodePIDPressure, v1.ConditionFalse, "KubeletHasSufficientPID", "kubelet has sufficient PID available"),
		cond(v1.NodeReady, v1.ConditionTrue, "KubeletReady", "kubelet is posting ready status from a browser tab"),
	}
	node.Status.Addresses = []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: tabNodeIP},
		{Type: v1.NodeHostName, Address: tabNodeName},
	}
	node.Status.DaemonEndpoints.KubeletEndpoint.Port = kubeletPort
	node.Status.NodeInfo = v1.NodeSystemInfo{
		MachineID:               "tabernetes",
		SystemUUID:              string(n.nodeUID),
		BootID:                  bootID,
		KernelVersion:           "WebAssembly (" + runtime.Version() + ")",
		OSImage:                 ua + " (browser tab)",
		ContainerRuntimeVersion: "tabernetes://" + version.Get().GitVersion,
		KubeletVersion:          version.Get().GitVersion,
		OperatingSystem:         "linux",
		Architecture:            "wasm",
	}
	node.Status.Images = builtinImageList()
	_, err = n.client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	return err
}

var bootID = randToken(16)

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
