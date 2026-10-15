package inventory

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func harvesterMachine(ns, name string, labels map[string]string, vmNamespace, providerID string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rke-machine.cattle.io/v1",
		"kind":       "HarvesterMachine",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{"vmNamespace": vmNamespace, "providerID": providerID},
	}}
	u.SetLabels(labels)
	return u
}

func fakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			HarvesterMachineGVR: "HarvesterMachineList",
			VirtualMachineGVR:   "VirtualMachineList",
		}, objs...)
}

func cpLabels(cluster, pool string) map[string]string {
	return map[string]string{
		LabelClusterName: cluster,
		LabelPoolName:    pool,
		LabelEtcdRole:    "true",
		LabelCPRole:      "true",
	}
}

func TestRancherSourceMembers(t *testing.T) {
	deleting := harvesterMachine("fleet-default", "stl-west-1-cp-gone", cpLabels("stl-west-1", "control-plane"), "sicura", "")
	now := metav1.Now()
	deleting.SetDeletionTimestamp(&now)

	client := fakeDynamic(
		harvesterMachine("fleet-default", "stl-west-1-cp-b", cpLabels("stl-west-1", "control-plane"), "sicura", "harvester://uid-b"),
		harvesterMachine("fleet-default", "stl-west-1-cp-a", cpLabels("stl-west-1", "control-plane"), "sicura", "harvester://uid-a"),
		harvesterMachine("fleet-default", "stl-west-1-w-a", map[string]string{
			LabelClusterName: "stl-west-1",
			LabelDeployment:  "stl-west-1-workers",
			LabelWorkerRole:  "true",
		}, "sicura", ""),
		harvesterMachine("fleet-default", "no-vm-namespace", cpLabels("x", "y"), "", ""),
		harvesterMachine("fleet-default", "no-labels", nil, "sicura", ""),
		harvesterMachine("other-ns", "other", cpLabels("other", "pool"), "default", ""),
		deleting,
	)

	src := &RancherSource{Client: client, Namespace: "fleet-default"}
	got, err := src.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Member{
		{Cluster: "stl-west-1", Pool: "control-plane", Namespace: "sicura", Name: "stl-west-1-cp-a", UID: "uid-a", Roles: []string{RoleControlPlane, RoleEtcd}},
		{Cluster: "stl-west-1", Pool: "control-plane", Namespace: "sicura", Name: "stl-west-1-cp-b", UID: "uid-b", Roles: []string{RoleControlPlane, RoleEtcd}},
		{Cluster: "stl-west-1", Pool: "stl-west-1-workers", Namespace: "sicura", Name: "stl-west-1-w-a", Roles: []string{RoleWorker}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestRancherSourceAllNamespacesAndFilter(t *testing.T) {
	client := fakeDynamic(
		harvesterMachine("fleet-default", "a", cpLabels("keep", "p"), "ns", ""),
		harvesterMachine("other-ns", "b", cpLabels("keep", "p"), "ns", ""),
		harvesterMachine("fleet-default", "c", cpLabels("drop", "p"), "ns", ""),
	)
	src := &RancherSource{Client: client, Filter: Filter{Exclude: []string{"drop"}}}
	got, err := src.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("unexpected members %+v", got)
	}

	src.Filter = Filter{Include: []string{"drop"}}
	got, err = src.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "c" {
		t.Fatalf("unexpected members %+v", got)
	}
}

func TestLabelSourceMembers(t *testing.T) {
	vm := func(ns, name, uid string, labels map[string]string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine"}}
		u.SetNamespace(ns)
		u.SetName(name)
		u.SetUID(ktypes.UID(uid))
		u.SetLabels(labels)
		return u
	}
	client := fakeDynamic(
		vm("sicura", "cp-1", "u1", map[string]string{LabelMachineSet: "sicura-stl-west-1-control-plane", LabelGuestCluster: "stl-west-1"}),
		vm("home", "gitea", "u2", nil),
	)
	src := &LabelSource{Client: client}
	got, err := src.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Member{{Cluster: "stl-west-1", Pool: "sicura-stl-west-1-control-plane", Namespace: "sicura", Name: "cp-1", UID: "u1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestMemberPriorityAndGroup(t *testing.T) {
	cases := []struct {
		roles []string
		want  int
	}{
		{[]string{RoleControlPlane, RoleEtcd}, 0},
		{[]string{RoleEtcd}, 0},
		{[]string{RoleControlPlane}, 1},
		{[]string{RoleWorker}, 2},
		{nil, 2},
	}
	for _, c := range cases {
		if got := (Member{Roles: c.roles}).Priority(); got != c.want {
			t.Errorf("roles %v: got %d want %d", c.roles, got, c.want)
		}
	}
	if g := (Member{Cluster: "c", Pool: "p"}).Group(); g != "c/p" {
		t.Errorf("group = %q", g)
	}
}
