package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestKubectlAccessName(t *testing.T) {
	for _, n := range []int{1, 2, 10, 250} {
		if name := kubectlAccessName(n); !validKubectlAccessName(name) {
			t.Errorf("%s is not valid", name)
		}
	}
	// revokeKubectl deletes by this name: nothing else may pass.
	for _, bad := range []string{"", "kubectl-", "kubectl-0", "kubectl-01", "kubectl-1,x", "kubectl-1 -A",
		"node-1", "tab", "kubectl-99999999", "kubectl--1", "kubectl-1/x"} {
		if validKubectlAccessName(bad) {
			t.Errorf("%q is valid", bad)
		}
	}
}

func TestKubectlAccessManifest(t *testing.T) {
	docs := strings.Split(kubectlAccessManifest("kubectl-3"), "\n---\n")
	if len(docs) != 2 {
		t.Fatalf("%d documents", len(docs))
	}
	var sa corev1.ServiceAccount
	if err := yaml.UnmarshalStrict([]byte(docs[0]), &sa); err != nil {
		t.Fatal(err)
	}
	var crb rbacv1.ClusterRoleBinding
	if err := yaml.UnmarshalStrict([]byte(docs[1]), &crb); err != nil {
		t.Fatal(err)
	}
	if sa.Kind != "ServiceAccount" || sa.Name != "tabernetes-kubectl-3" || sa.Namespace != kubectlAccessNS ||
		sa.Labels[kubectlAccessLabel] != "kubectl-3" {
		t.Errorf("ServiceAccount: %+v", sa.ObjectMeta)
	}
	if crb.Kind != "ClusterRoleBinding" || crb.Name != "tabernetes-kubectl-3" || crb.Labels[kubectlAccessLabel] != "kubectl-3" ||
		crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != "cluster-admin" {
		t.Errorf("ClusterRoleBinding: %+v %+v", crb.ObjectMeta, crb.RoleRef)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0] != (rbacv1.Subject{Kind: "ServiceAccount", Name: sa.Name, Namespace: sa.Namespace}) {
		t.Errorf("subjects: %+v", crb.Subjects)
	}
}
