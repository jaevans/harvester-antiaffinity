package harvester

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/jaevans/harvester-antiaffinity/internal/inventory"
	"github.com/jaevans/harvester-antiaffinity/internal/planner"
)

var upgradeGVK = schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "Upgrade"}

func schemeWithUpgrades() *runtime.Scheme {
	s := NewScheme()
	s.AddKnownTypeWithName(upgradeGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(upgradeGVK.GroupVersion().WithKind("UpgradeList"), &unstructured.UnstructuredList{})
	return s
}

func readyNode(name string, memGi int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(memGi<<30, resource.BinarySI)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func runningVM(ns, name, uid, node string) (*kubevirtv1.VirtualMachine, *kubevirtv1.VirtualMachineInstance, *corev1.Pod) {
	vm := &kubevirtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)}}
	podUID := types.UID("pod-" + name)
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			Phase:      kubevirtv1.Running,
			NodeName:   node,
			ActivePods: map[types.UID]string{podUID: node},
			Conditions: []kubevirtv1.VirtualMachineInstanceCondition{
				{Type: kubevirtv1.VirtualMachineInstanceReady, Status: corev1.ConditionTrue},
				{Type: kubevirtv1.VirtualMachineInstanceIsMigratable, Status: corev1.ConditionTrue},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "virt-launcher-" + name, UID: podUID},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{Name: "compute", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	return vm, vmi, pod
}

func newClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(schemeWithUpgrades()).WithObjects(objs...).WithStatusSubresource(&kubevirtv1.VirtualMachineInstanceMigration{}).Build()
}

func member(ns, name, uid string) inventory.Member {
	return inventory.Member{Cluster: "c", Pool: "cp", Namespace: ns, Name: name, UID: uid, Roles: []string{inventory.RoleEtcd}}
}

func TestCollectBasic(t *testing.T) {
	vm1, vmi1, pod1 := runningVM("ns", "vm1", "uid1", "a")
	vm2, vmi2, pod2 := runningVM("ns", "vm2", "uid2", "a")
	c := &Collector{Client: newClient(readyNode("a", 64), readyNode("b", 64), vm1, vmi1, pod1, vm2, vmi2, pod2)}

	st, err := c.Collect(context.Background(), []inventory.Member{
		member("ns", "vm1", "uid1"),
		member("ns", "vm2", "uid2"),
		member("ns", "elsewhere", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Busy != "" {
		t.Fatalf("unexpected busy: %s", st.Busy)
	}
	wantNodes := []planner.Node{{Name: "a", FreeMemory: 60 << 30}, {Name: "b", FreeMemory: 64 << 30}}
	if !reflect.DeepEqual(st.Nodes, wantNodes) {
		t.Fatalf("nodes: got %+v want %+v", st.Nodes, wantNodes)
	}
	if len(st.VMs) != 2 {
		t.Fatalf("expected 2 VMs, got %+v", st.VMs)
	}
	got := st.VMs[0]
	want := planner.VM{Namespace: "ns", Name: "vm1", Group: "c/cp", Priority: 0, Node: "a", Movable: true, MemoryRequest: 2 << 30, AllowedNodes: []string{"a", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vm: got %+v want %+v", got, want)
	}
	if len(st.Skipped) != 1 || st.Skipped[0].Name != "elsewhere" {
		t.Fatalf("expected the missing VM to be skipped, got %+v", st.Skipped)
	}

	moves := planner.Plan(st.Nodes, st.VMs, planner.Options{})
	if len(moves) != 1 || moves[0].To != "b" {
		t.Fatalf("expected one move to b, got %v", moves)
	}
}

func TestCollectUIDMismatchAndStopped(t *testing.T) {
	vm1, vmi1, pod1 := runningVM("ns", "vm1", "uid1", "a")
	stopped := &kubevirtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "stopped", UID: "uid-s"}}
	c := &Collector{Client: newClient(readyNode("a", 64), vm1, vmi1, pod1, stopped)}
	st, err := c.Collect(context.Background(), []inventory.Member{member("ns", "vm1", "other-uid"), member("ns", "stopped", "uid-s")})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.VMs) != 0 || len(st.Skipped) != 2 {
		t.Fatalf("expected both skipped, got VMs=%+v skipped=%+v", st.VMs, st.Skipped)
	}
}

func TestNodeEligibility(t *testing.T) {
	cordoned := readyNode("cordoned", 64)
	cordoned.Spec.Unschedulable = true
	maint := readyNode("maint", 64)
	maint.Annotations = map[string]string{AnnotationMaintainStatus: "completed"}
	notReady := readyNode("notready", 64)
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	tainted := readyNode("tainted", 64)
	tainted.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "x", Effect: corev1.TaintEffectNoSchedule}}
	prefer := readyNode("prefer", 64)
	prefer.Spec.Taints = []corev1.Taint{{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule}}
	labelled := readyNode("labelled", 64)
	labelled.Labels["network.harvesterhci.io/data"] = "true"

	vm, vmi, pod := runningVM("ns", "vm", "uid", "labelled")
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "network.harvesterhci.io/data", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}},
		}}},
	}}
	vm2, vmi2, pod2 := runningVM("ns", "vm2", "uid2", "labelled")
	pod2.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}
	_ = vmi2

	c := &Collector{Client: newClient(cordoned, maint, notReady, tainted, prefer, labelled, vm, vmi, pod, vm2, vmi2, pod2)}
	st, err := c.Collect(context.Background(), []inventory.Member{member("ns", "vm", "uid"), member("ns", "vm2", "uid2")})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range st.Nodes {
		names = append(names, n.Name)
	}
	if !reflect.DeepEqual(names, []string{"labelled", "prefer", "tainted"}) {
		t.Fatalf("eligible nodes = %v", names)
	}
	if !reflect.DeepEqual(st.VMs[0].AllowedNodes, []string{"labelled"}) {
		t.Fatalf("node affinity not applied: %v", st.VMs[0].AllowedNodes)
	}
	if !reflect.DeepEqual(st.VMs[1].AllowedNodes, []string{"labelled", "prefer", "tainted"}) {
		t.Fatalf("toleration not applied: %v", st.VMs[1].AllowedNodes)
	}
}

func TestMovableReasons(t *testing.T) {
	base := func() (*kubevirtv1.VirtualMachine, *kubevirtv1.VirtualMachineInstance) {
		vm, vmi, _ := runningVM("ns", "vm", "uid", "a")
		return vm, vmi
	}
	cases := map[string]func(*kubevirtv1.VirtualMachine, *kubevirtv1.VirtualMachineInstance) bool{
		"ok": func(*kubevirtv1.VirtualMachine, *kubevirtv1.VirtualMachineInstance) bool { return false },
		"opt-out on VM": func(vm *kubevirtv1.VirtualMachine, _ *kubevirtv1.VirtualMachineInstance) bool {
			vm.Annotations = map[string]string{AnnotationIgnore: "true"}
			return true
		},
		"not live migratable": func(_ *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
			vmi.Status.Conditions[1].Status = corev1.ConditionFalse
			return true
		},
		"not ready": func(_ *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
			vmi.Status.Conditions[0].Status = corev1.ConditionFalse
			return true
		},
		"paused phase": func(_ *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
			vmi.Status.Phase = kubevirtv1.Scheduling
			return true
		},
		"migration state in progress": func(_ *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
			vmi.Status.MigrationState = &kubevirtv1.VirtualMachineInstanceMigrationState{}
			return true
		},
		"previous migration completed": func(_ *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
			vmi.Status.MigrationState = &kubevirtv1.VirtualMachineInstanceMigrationState{Completed: true}
			return false
		},
	}
	for name, mutate := range cases {
		vm, vmi := base()
		wantBlocked := mutate(vm, vmi)
		if got := movable(vm, vmi, false) != ""; got != wantBlocked {
			t.Errorf("%s: blocked=%v want %v", name, got, wantBlocked)
		}
	}
	vm, vmi := base()
	if movable(vm, vmi, true) == "" {
		t.Errorf("active VMIM should block")
	}
}

func TestCollectBusyOnOwnActiveMigration(t *testing.T) {
	vm1, vmi1, pod1 := runningVM("ns", "vm1", "uid1", "a")
	ours := &kubevirtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1", Labels: map[string]string{LabelManagedBy: ManagedByValue}},
		Spec:       kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: "vm1"},
		Status:     kubevirtv1.VirtualMachineInstanceMigrationStatus{Phase: kubevirtv1.MigrationRunning},
	}
	c := &Collector{Client: newClient(readyNode("a", 64), vm1, vmi1, pod1, ours)}
	st, err := c.Collect(context.Background(), []inventory.Member{member("ns", "vm1", "uid1")})
	if err != nil {
		t.Fatal(err)
	}
	if st.Busy == "" {
		t.Fatalf("expected busy because our own migration is running")
	}
	if st.VMs[0].Movable {
		t.Fatalf("VM with active migration must not be movable")
	}

	ours.Status.Phase = kubevirtv1.MigrationSucceeded
	c = &Collector{Client: newClient(readyNode("a", 64), vm1, vmi1, pod1, ours)}
	if st, _ = c.Collect(context.Background(), nil); st.Busy != "" {
		t.Fatalf("finished migration should not mark busy: %s", st.Busy)
	}
}

func upgrade(name string, labels map[string]string, completed string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(upgradeGVK)
	u.SetNamespace("harvester-system")
	u.SetName(name)
	u.SetLabels(labels)
	if completed != "" {
		_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Completed", "status": completed}}, "status", "conditions")
	}
	return u
}

func TestUpgradeInProgress(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		busy bool
	}{
		{"succeeded label", upgrade("u", map[string]string{LabelUpgradeState: "Succeeded"}, ""), false},
		{"failed label", upgrade("u", map[string]string{LabelUpgradeState: "Failed"}, ""), false},
		{"completed condition", upgrade("u", nil, "True"), false},
		{"running", upgrade("u", map[string]string{LabelUpgradeState: "UpgradingNodes"}, "Unknown"), true},
		{"fresh", upgrade("u", nil, ""), true},
	}
	for _, tc := range cases {
		c := &Collector{Client: newClient(tc.obj)}
		st, err := c.Collect(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if (st.Busy != "") != tc.busy {
			t.Errorf("%s: busy=%q want busy=%v", tc.name, st.Busy, tc.busy)
		}
	}
}

func TestUpgradeCRDAbsentIsIgnored(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).Build()
	st, err := (&Collector{Client: cl}).Collect(context.Background(), nil)
	if err != nil {
		t.Fatalf("missing upgrade CRD should be ignored, got %v", err)
	}
	if st.Busy != "" {
		t.Fatalf("unexpected busy %q", st.Busy)
	}
}

func TestFreeMemoryIgnoresTerminalPodsAndCountsOverhead(t *testing.T) {
	n := readyNode("a", 10)
	mk := func(name string, phase corev1.PodPhase, req string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PodSpec{NodeName: "a", Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(req)},
			}}}},
			Status: corev1.PodStatus{Phase: phase},
		}
	}
	withOverhead := mk("o", corev1.PodRunning, "1Gi")
	withOverhead.Spec.Overhead = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	pods := []corev1.Pod{mk("r", corev1.PodRunning, "1Gi"), mk("done", corev1.PodSucceeded, "4Gi"), mk("failed", corev1.PodFailed, "4Gi"), withOverhead}
	free := freeMemory([]*corev1.Node{n}, pods)
	if want := int64(7 << 30); free["a"] != want {
		t.Fatalf("free = %d, want %d", free["a"], want)
	}
}

func TestLauncherMemoryFallbacks(t *testing.T) {
	_, vmi, _ := runningVM("ns", "vm", "uid", "a")
	if got := launcherMemory(vmi, nil); got != 0 {
		t.Fatalf("no data should be 0, got %d", got)
	}
	guest := resource.MustParse("4Gi")
	vmi.Spec.Domain.Memory = &kubevirtv1.Memory{Guest: &guest}
	if got := launcherMemory(vmi, nil); got != 4<<30 {
		t.Fatalf("guest fallback: got %d", got)
	}
	vmi.Spec.Domain.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")}
	if got := launcherMemory(vmi, nil); got != 3<<30 {
		t.Fatalf("request fallback: got %d", got)
	}
}

func TestAllowedNodesFallsBackToVMISpec(t *testing.T) {
	a, b := readyNode("a", 64), readyNode("b", 64)
	b.Labels["zone"] = "x"
	_, vmi, _ := runningVM("ns", "vm", "uid", "a")
	vmi.Spec.NodeSelector = map[string]string{"zone": "x"}
	if got := allowedNodes(vmi, nil, []*corev1.Node{a, b}); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("got %v", got)
	}
}

func TestAllowedNodesUsesLauncherPodSelectors(t *testing.T) {
	// KubeVirt puts kubevirt.io/schedulable on the launcher pod, not the VMI.
	a, b := readyNode("a", 64), readyNode("b", 64)
	a.Labels["kubevirt.io/schedulable"] = "true"
	_, vmi, pod := runningVM("ns", "vm", "uid", "a")
	pod.Spec.NodeSelector = map[string]string{"kubevirt.io/schedulable": "true"}
	pods := map[string]*corev1.Pod{string(pod.UID): pod}
	if got := allowedNodes(vmi, pods, []*corev1.Node{a, b}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCollectBusyOnAnyUnfinishedMigration(t *testing.T) {
	vm1, vmi1, pod1 := runningVM("ns", "vm1", "uid1", "a")
	now := metav1.Now()
	cases := map[string]*kubevirtv1.VirtualMachineInstanceMigration{
		"external running": {
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "evac"},
			Spec:       kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: "other"},
			Status:     kubevirtv1.VirtualMachineInstanceMigrationStatus{Phase: kubevirtv1.MigrationScheduling},
		},
		"ours being deleted": {
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "aborting", DeletionTimestamp: &now, Finalizers: []string{"kubevirt.io/migrationJobFinalize"},
				Labels: map[string]string{LabelManagedBy: ManagedByValue}},
			Spec:   kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: "vm1"},
			Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{Phase: kubevirtv1.MigrationRunning},
		},
	}
	for name, m := range cases {
		c := &Collector{Client: newClient(readyNode("a", 64), vm1, vmi1, pod1, m)}
		st, err := c.Collect(context.Background(), []inventory.Member{member("ns", "vm1", "uid1")})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Busy == "" {
			t.Errorf("%s: expected busy", name)
		}
	}
}

func TestCollectBusyWhileNodeEntersMaintenance(t *testing.T) {
	n := readyNode("a", 64)
	n.Annotations = map[string]string{AnnotationMaintainStatus: "running"}
	st, err := (&Collector{Client: newClient(n)}).Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Busy == "" {
		t.Fatalf("expected busy while a node drains")
	}
}

func TestCollectSkipsMemberWithoutUID(t *testing.T) {
	vm1, vmi1, pod1 := runningVM("ns", "vm1", "uid1", "a")
	st, err := (&Collector{Client: newClient(readyNode("a", 64), vm1, vmi1, pod1)}).Collect(context.Background(), []inventory.Member{member("ns", "vm1", "")})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.VMs) != 0 || len(st.Skipped) != 1 {
		t.Fatalf("member without providerID must be skipped: %+v", st)
	}
}
