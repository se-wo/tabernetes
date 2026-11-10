package main

// rancher/local-path-provisioner as a built-in image, for the tab node and
// the nodes in Web Workers. It does what the real one does: for a claim of a
// StorageClass of the provisioner rancher.io/local-path it starts a helper
// pod on the node the scheduler picked (the setup script of the ConfigMap
// local-path-config makes a directory per claim), creates a hostPath
// PersistentVolume with node affinity to that node, and for a released
// volume with reclaimPolicy Delete it starts a helper pod with the teardown
// script and deletes the volume. A node has its own file system, so a helper
// pod is the only way to reach the directory on every kind of node.

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"encoding/json"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"mvdan.cc/sh/v3/interp"
	"sigs.k8s.io/yaml"
)

const (
	localPathProvisioner = "rancher.io/local-path"
	// defaultNodePathKey is the node name for "every node not listed".
	defaultNodePathKey = "DEFAULT_PATH_FOR_NON_LISTED_NODES"
	storageProvisioner = "volume.kubernetes.io/storage-provisioner"
	betaProvisioner    = "volume.beta.kubernetes.io/storage-provisioner"
	selectedNodeAnno   = "volume.kubernetes.io/selected-node"
	provisionedByAnno  = "pv.kubernetes.io/provisioned-by"
	helperPodTimeout   = 2 * time.Minute
)

func init() {
	imagePrograms["local-path-provisioner"] = localPathProg
	imageDefs = append(imageDefs, &imageDef{
		img:   builtinImage{name: "rancher/local-path-provisioner", entrypoint: []string{"local-path-provisioner"}, files: alpineFiles},
		repos: []string{"rancher/local-path-provisioner"}, tag: "v0.0.31", size: 18_652_343,
	})
}

// localPathConfig is config.json of the ConfigMap local-path-config.
type localPathConfig struct {
	NodePathMap []struct {
		Node  string   `json:"node"`
		Paths []string `json:"paths"`
	} `json:"nodePathMap"`
}

// basePath is the directory for the volumes of a node: the paths of its entry
// or else those of the default entry.
func (c *localPathConfig) basePath(node string) (string, error) {
	for _, key := range []string{node, defaultNodePathKey} {
		for _, e := range c.NodePathMap {
			if e.Node == key && len(e.Paths) > 0 {
				return e.Paths[0], nil
			}
		}
	}
	return "", fmt.Errorf("no path for node %s in nodePathMap", node)
}

type localPath struct {
	cs     kubernetes.Interface
	ns     string
	cfgDir string // the directory of config.json, setup, teardown and helperPod.yaml
	pod    string
	log    func(level, format string, a ...any)
	busy   sync.Map // claim UID or volume name -> struct{}
	poll   time.Duration
}

// localPathProg is the command line of the real binary:
// local-path-provisioner [--debug] start --config /etc/config/config.json
func localPathProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	cfgFile := "/etc/config/config.json"
	start := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "start":
			start = true
		case (a == "--config" || a == "-c") && i+1 < len(args):
			cfgFile = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			cfgFile = strings.TrimPrefix(a, "--config=")
		}
	}
	logf := func(level, format string, a ...any) {
		fmt.Fprintf(hc.Stdout, "time=%q level=%s msg=%q\n", time.Now().UTC().Format(time.RFC3339), level, fmt.Sprintf(format, a...))
	}
	if !start {
		fmt.Fprintln(hc.Stdout, "NAME:\n   local-path-provisioner - A local path provisioner\n\nUSAGE:\n   local-path-provisioner [global options] command [command options] [arguments...]\n\nCOMMANDS:\n   start    Start provisioner\n   help, h  Shows a list of commands or help for one command")
		return nil
	}
	pe := procOf(ctx)
	if pe == nil {
		return fmt.Errorf("not in a pod")
	}
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	ns := getenv(hc, "POD_NAMESPACE")
	if b, err := os.ReadFile(pe.root.real(saDir + "/namespace")); err == nil && ns == "" {
		ns = strings.TrimSpace(string(b))
	}
	cs, err := kubernetes.NewForConfig(&rest.Config{
		Host:            "https://10.96.0.1:443",
		TLSClientConfig: rest.TLSClientConfig{CAFile: pe.root.real(saDir + "/ca.crt")},
		BearerTokenFile: pe.root.real(saDir + "/token"),
		QPS:             50, Burst: 100,
	})
	if err != nil {
		logf("fatal", "Error starting daemon: %v", err)
		return interp.ExitStatus(1)
	}
	lp := &localPath{cs: cs, ns: ns, cfgDir: path.Dir(pe.root.real(cfgFile)), pod: podHostname(ctx), log: logf, poll: time.Second}
	if _, err := lp.config(); err != nil {
		logf("fatal", "Error starting daemon: %v", err)
		return interp.ExitStatus(1)
	}
	logf("debug", "Provisioner started")
	lp.run(ctx)
	return nil
}

func (lp *localPath) config() (*localPathConfig, error) {
	b, err := os.ReadFile(filepath.Join(lp.cfgDir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	var c localPathConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("invalid config.json: %w", err)
	}
	return &c, nil
}

// run is the control loop: it provisions the claims that wait for a volume and
// deletes the released volumes. The real one is driven by informers; a list
// each second is as good for a cluster of one user.
func (lp *localPath) run(ctx context.Context) {
	t := time.NewTicker(lp.poll)
	defer t.Stop()
	for {
		lp.sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (lp *localPath) sync(ctx context.Context) {
	scs, err := lp.cs.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	classes := map[string]*storagev1.StorageClass{}
	for i := range scs.Items {
		if scs.Items[i].Provisioner == localPathProvisioner {
			classes[scs.Items[i].Name] = &scs.Items[i]
		}
	}
	if len(classes) == 0 {
		return
	}
	pvcs, err := lp.cs.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	pvs, err := lp.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	have := map[string]bool{}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		have[pv.Name] = true
		if pv.Annotations[provisionedByAnno] != localPathProvisioner {
			continue
		}
		if (pv.Status.Phase == v1.VolumeReleased || pv.Status.Phase == v1.VolumeFailed) &&
			pv.Spec.PersistentVolumeReclaimPolicy == v1.PersistentVolumeReclaimDelete && lp.claim(pv.Name) {
			go func() {
				defer lp.busy.Delete(pv.Name)
				lp.deleteVolume(ctx, pv)
			}()
		}
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		class := classes[claimClass(pvc)]
		if class == nil || pvc.Spec.VolumeName != "" || pvc.DeletionTimestamp != nil || have["pvc-"+string(pvc.UID)] {
			continue
		}
		if pvc.Annotations[storageProvisioner] != localPathProvisioner && pvc.Annotations[betaProvisioner] != localPathProvisioner {
			continue
		}
		// WaitForFirstConsumer: the scheduler has not picked a node yet.
		if pvc.Annotations[selectedNodeAnno] == "" && class.VolumeBindingMode != nil && *class.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer {
			continue
		}
		if lp.claim(string(pvc.UID)) {
			go func() {
				defer lp.busy.Delete(string(pvc.UID))
				lp.provision(ctx, pvc, class)
			}()
		}
	}
}

// claim marks key as being worked on; false when it already is.
func (lp *localPath) claim(key string) bool {
	_, loaded := lp.busy.LoadOrStore(key, struct{}{})
	return !loaded
}

func claimClass(pvc *v1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName != nil {
		return *pvc.Spec.StorageClassName
	}
	return pvc.Annotations[v1.BetaStorageClassAnnotation]
}

func (lp *localPath) event(ctx context.Context, pvc *v1.PersistentVolumeClaim, typ, reason, msg string) {
	now := metav1.Now()
	_, _ = lp.cs.CoreV1().Events(pvc.Namespace).Create(ctx, &v1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x", pvc.Name, now.UnixNano()), Namespace: pvc.Namespace},
		InvolvedObject: v1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: pvc.Namespace, Name: pvc.Name,
			UID: pvc.UID, APIVersion: "v1", ResourceVersion: pvc.ResourceVersion},
		Reason: reason, Message: msg, Type: typ,
		Source:         v1.EventSource{Component: localPathProvisioner + "_" + lp.pod},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
		ReportingController: localPathProvisioner, ReportingInstance: lp.pod,
	}, metav1.CreateOptions{})
}

func (lp *localPath) provision(ctx context.Context, pvc *v1.PersistentVolumeClaim, class *storagev1.StorageClass) {
	pvName := "pvc-" + string(pvc.UID)
	lp.event(ctx, pvc, v1.EventTypeNormal, "Provisioning", fmt.Sprintf("External provisioner is provisioning volume for claim %q", pvc.Namespace+"/"+pvc.Name))
	vol, err := lp.createVolume(ctx, pvc, class, pvName)
	if err != nil {
		lp.log("error", "failed to provision volume with StorageClass %q: %v", class.Name, err)
		lp.event(ctx, pvc, v1.EventTypeWarning, "ProvisioningFailed", fmt.Sprintf("failed to provision volume with StorageClass %q: %v", class.Name, err))
		return
	}
	if _, err := lp.cs.CoreV1().PersistentVolumes().Create(ctx, vol, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		lp.log("error", "failed to create volume %s: %v", pvName, err)
		lp.event(ctx, pvc, v1.EventTypeWarning, "ProvisioningFailed", fmt.Sprintf("failed to save volume %s: %v", pvName, err))
		return
	}
	lp.event(ctx, pvc, v1.EventTypeNormal, "ProvisioningSucceeded", "Successfully provisioned volume "+pvName)
}

func (lp *localPath) createVolume(ctx context.Context, pvc *v1.PersistentVolumeClaim, class *storagev1.StorageClass, pvName string) (*v1.PersistentVolume, error) {
	node := pvc.Annotations[selectedNodeAnno]
	if node == "" {
		return nil, fmt.Errorf("configuration error, no node was specified")
	}
	if _, err := lp.cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{}); err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", node, err)
	}
	cfg, err := lp.config()
	if err != nil {
		return nil, err
	}
	base, err := cfg.basePath(node)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, pvName+"_"+pvc.Namespace+"_"+pvc.Name)
	size := pvc.Spec.Resources.Requests[v1.ResourceStorage]
	mode := v1.PersistentVolumeFilesystem
	if pvc.Spec.VolumeMode != nil {
		mode = *pvc.Spec.VolumeMode
	}
	if mode == v1.PersistentVolumeBlock {
		return nil, fmt.Errorf("NodePath only supports %s volume", v1.PersistentVolumeFilesystem)
	}
	lp.log("info", "Creating volume %s at %s:%s", pvName, node, dir)
	if err := lp.helper(ctx, "setup", pvName, node, dir, size.Value()); err != nil {
		return nil, err
	}
	lp.log("info", "Volume %s has been created on %s:%s", pvName, node, dir)
	reclaim := v1.PersistentVolumeReclaimDelete
	if class.ReclaimPolicy != nil {
		reclaim = *class.ReclaimPolicy
	}
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName, Annotations: map[string]string{provisionedByAnno: localPathProvisioner}},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: reclaim,
			AccessModes:                   pvc.Spec.AccessModes,
			VolumeMode:                    &mode,
			Capacity:                      v1.ResourceList{v1.ResourceStorage: size},
			StorageClassName:              class.Name,
			MountOptions:                  class.MountOptions,
			ClaimRef: &v1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: pvc.Namespace,
				Name: pvc.Name, UID: pvc.UID, ResourceVersion: pvc.ResourceVersion},
			PersistentVolumeSource: v1.PersistentVolumeSource{HostPath: &v1.HostPathVolumeSource{
				Path: dir, Type: ptr.To(v1.HostPathDirectoryOrCreate)}},
			NodeAffinity: &v1.VolumeNodeAffinity{Required: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{{
				MatchExpressions: []v1.NodeSelectorRequirement{{Key: v1.LabelHostname, Operator: v1.NodeSelectorOpIn, Values: []string{node}}}}}}},
		},
	}, nil
}

func (lp *localPath) deleteVolume(ctx context.Context, pv *v1.PersistentVolume) {
	var dir, node string
	switch {
	case pv.Spec.HostPath != nil:
		dir = pv.Spec.HostPath.Path
	case pv.Spec.Local != nil:
		dir = pv.Spec.Local.Path
	}
	if na := pv.Spec.NodeAffinity; na != nil && na.Required != nil {
		for _, t := range na.Required.NodeSelectorTerms {
			for _, e := range t.MatchExpressions {
				if e.Key == v1.LabelHostname && len(e.Values) == 1 {
					node = e.Values[0]
				}
			}
		}
	}
	if dir == "" || node == "" {
		lp.log("error", "failed to delete volume %s: no path or node", pv.Name)
		return
	}
	lp.log("info", "Deleting volume %s at %s:%s", pv.Name, node, dir)
	if err := lp.helper(ctx, "teardown", pv.Name, node, dir, 0); err != nil {
		lp.log("error", "failed to delete volume %s: %v", pv.Name, err)
		return
	}
	if err := lp.cs.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		lp.log("error", "failed to delete volume %s: %v", pv.Name, err)
		return
	}
	lp.log("info", "Volume %s has been deleted on %s:%s", pv.Name, node, dir)
}

// helper runs the script (setup or teardown) of the ConfigMap in a pod on the
// node, with the parent of dir mounted at /data, and waits for it to end.
func (lp *localPath) helper(ctx context.Context, script, pvName, node, dir string, size int64) error {
	tpl, err := os.ReadFile(filepath.Join(lp.cfgDir, "helperPod.yaml"))
	if err != nil {
		return fmt.Errorf("failed to read helperPod.yaml: %w", err)
	}
	pod := &v1.Pod{}
	if err := yaml.Unmarshal(tpl, pod); err != nil || len(pod.Spec.Containers) == 0 {
		return fmt.Errorf("invalid helperPod.yaml: %v", err)
	}
	action := map[string]string{"setup": "create", "teardown": "delete"}[script]
	pod.Name = "helper-pod-" + action + "-" + pvName
	pod.Namespace = lp.ns
	pod.Spec.NodeName = node
	pod.Spec.RestartPolicy = v1.RestartPolicyNever
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, v1.Toleration{Operator: v1.TolerationOpExists})
	pod.Spec.Volumes = []v1.Volume{
		{Name: "data", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{
			Path: path.Dir(dir), Type: ptr.To(v1.HostPathDirectoryOrCreate)}}},
		{Name: "script", VolumeSource: v1.VolumeSource{ConfigMap: &v1.ConfigMapVolumeSource{
			LocalObjectReference: v1.LocalObjectReference{Name: "local-path-config"},
			Items:                []v1.KeyToPath{{Key: "setup", Path: "setup"}, {Key: "teardown", Path: "teardown"}}}}},
	}
	c := &pod.Spec.Containers[0]
	c.Command = []string{"/bin/sh", "/script/" + script}
	c.Env = []v1.EnvVar{
		{Name: "VOL_DIR", Value: "/data/" + path.Base(dir)},
		{Name: "VOL_MODE", Value: string(v1.PersistentVolumeFilesystem)},
		{Name: "VOL_SIZE_BYTES", Value: fmt.Sprint(size)},
	}
	c.VolumeMounts = []v1.VolumeMount{{Name: "data", MountPath: "/data/"}, {Name: "script", MountPath: "/script"}}
	pods := lp.cs.CoreV1().Pods(lp.ns)
	_ = pods.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})
	for i := 0; i < 20; i++ { // a left-over pod of an earlier try goes away first
		if _, err := pods.Get(ctx, pod.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create helper pod: %w", err)
	}
	defer pods.Delete(context.WithoutCancel(ctx), pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})
	deadline := time.Now().Add(helperPodTimeout)
	for time.Now().Before(deadline) {
		p, err := pods.Get(ctx, pod.Name, metav1.GetOptions{})
		if err == nil {
			switch p.Status.Phase {
			case v1.PodSucceeded:
				return nil
			case v1.PodFailed:
				return fmt.Errorf("%s process failed: helper pod %s failed", script, pod.Name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lp.poll / 2):
		}
	}
	return fmt.Errorf("%s process timed out: helper pod %s did not finish", script, pod.Name)
}
