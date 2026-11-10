package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

// localPathAddon returns the ConfigMap data and the StorageClasses of the addon.
func localPathAddon(t *testing.T) (map[string]string, []*storagev1.StorageClass) {
	t.Helper()
	b, err := addonFS.ReadFile("addons/local-path-storage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	var scs []*storagev1.StorageClass
	for _, doc := range strings.Split(string(b), "\n---\n") {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "ConfigMap":
			cm := &v1.ConfigMap{}
			if err := yaml.UnmarshalStrict([]byte(doc), cm); err != nil {
				t.Fatal(err)
			}
			data = cm.Data
		case "StorageClass":
			sc := &storagev1.StorageClass{}
			if err := yaml.UnmarshalStrict([]byte(doc), sc); err != nil {
				t.Fatal(err)
			}
			scs = append(scs, sc)
		}
	}
	return data, scs
}

func TestLocalPathAddon(t *testing.T) {
	data, scs := localPathAddon(t)
	if len(scs) != 2 || scs[0].Name != "local-path" || scs[0].Annotations["storageclass.kubernetes.io/is-default-class"] != "true" {
		t.Fatalf("storage classes: %+v", scs)
	}
	if *scs[0].ReclaimPolicy != v1.PersistentVolumeReclaimDelete || *scs[1].ReclaimPolicy != v1.PersistentVolumeReclaimRetain {
		t.Fatal("reclaim policies")
	}
	for _, sc := range scs {
		if *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer || sc.Provisioner != localPathProvisioner {
			t.Fatalf("class %s: %+v", sc.Name, sc)
		}
	}
	var c localPathConfig
	if err := yaml.Unmarshal([]byte(data["config.json"]), &c); err != nil {
		t.Fatal(err)
	}
	if p, err := c.basePath("any-node"); err != nil || p != "/opt/local-path-provisioner" {
		t.Fatalf("basePath = %q, %v", p, err)
	}
	if lookupImage("rancher/local-path-provisioner:v0.0.31") == nil {
		t.Fatal("rancher/local-path-provisioner is not a built-in image")
	}
}

// The provisioner makes a volume for a claim that has a node, through a
// helper pod, and removes it again when the claim is gone.
func TestLocalPathProvisionAndDelete(t *testing.T) {
	data, scs := localPathAddon(t)
	dir := t.TempDir()
	for k, v := range data {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cs := fake.NewClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "tab"}}, scs[0], scs[1])
	lp := &localPath{cs: cs, ns: "local-path-storage", cfgDir: dir, pod: "p", poll: 20 * time.Millisecond,
		log: func(string, string, ...any) {}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A kubelet that runs helper pods to completion and records their scripts.
	scripts := make(chan string, 4)
	go func() {
		seen := map[string]bool{}
		for ctx.Err() == nil {
			pods, _ := cs.CoreV1().Pods(lp.ns).List(ctx, metav1.ListOptions{})
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.Status.Phase == v1.PodSucceeded || seen[p.Name] {
					continue
				}
				seen[p.Name] = true
				c := p.Spec.Containers[0]
				scripts <- p.Name + " " + p.Spec.NodeName + " " + strings.Join(c.Command, " ") + " " + c.Env[0].Value + " " + p.Spec.Volumes[0].HostPath.Path
				p.Status.Phase = v1.PodSucceeded
				cs.CoreV1().Pods(lp.ns).UpdateStatus(ctx, p, metav1.UpdateOptions{})
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	go lp.run(ctx)

	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default", UID: "u1", Annotations: map[string]string{storageProvisioner: localPathProvisioner}},
		Spec: v1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("local-path"), AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Gi")}}},
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("default").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// WaitForFirstConsumer: nothing happens until the scheduler picked a node.
	time.Sleep(200 * time.Millisecond)
	if pvs, _ := cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{}); len(pvs.Items) != 0 {
		t.Fatalf("volume before a consumer: %v", pvs.Items)
	}
	pvc.Annotations[selectedNodeAnno] = "tab"
	cs.CoreV1().PersistentVolumeClaims("default").Update(ctx, pvc, metav1.UpdateOptions{})

	select {
	case s := <-scripts:
		want := "helper-pod-create-pvc-u1 tab /bin/sh /script/setup /data/pvc-u1_default_data /opt/local-path-provisioner"
		if s != want {
			t.Fatalf("helper pod = %q, want %q", s, want)
		}
	case <-ctx.Done():
		t.Fatal("no setup helper pod")
	}
	var pv *v1.PersistentVolume
	for ctx.Err() == nil {
		pv, _ = cs.CoreV1().PersistentVolumes().Get(ctx, "pvc-u1", metav1.GetOptions{})
		if pv != nil && pv.Name != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pv == nil || pv.Name == "" {
		t.Fatal("no volume")
	}
	if pv.Spec.HostPath.Path != "/opt/local-path-provisioner/pvc-u1_default_data" || pv.Spec.StorageClassName != "local-path" ||
		pv.Spec.ClaimRef.Name != "data" || pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values[0] != "tab" ||
		pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimDelete {
		t.Fatalf("volume: %+v", pv.Spec)
	}

	// The volume controller marks the volume Released when the claim is gone.
	pv.Status.Phase = v1.VolumeReleased
	cs.CoreV1().PersistentVolumes().UpdateStatus(ctx, pv, metav1.UpdateOptions{})
	select {
	case s := <-scripts:
		if !strings.HasPrefix(s, "helper-pod-delete-pvc-u1 tab /bin/sh /script/teardown") {
			t.Fatalf("helper pod = %q", s)
		}
	case <-ctx.Done():
		t.Fatal("no teardown helper pod")
	}
	for ctx.Err() == nil {
		if pvs, _ := cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{}); len(pvs.Items) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("volume was not deleted")
}
