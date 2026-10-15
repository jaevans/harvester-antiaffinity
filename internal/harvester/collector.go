// Package harvester reads cluster state from Harvester and performs live
// migrations.
package harvester

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	v1helper "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jaevans/harvester-antiaffinity/internal/inventory"
	"github.com/jaevans/harvester-antiaffinity/internal/planner"
)

const (
	// AnnotationMaintainStatus is set by Harvester on nodes entering or in
	// maintenance mode.
	AnnotationMaintainStatus = "harvesterhci.io/maintain-status"
	// LabelUpgradeState is set by Harvester on Upgrade objects.
	LabelUpgradeState = "harvesterhci.io/upgradeState"
	// AnnotationIgnore on a VirtualMachine or VMI opts that VM out of being
	// migrated by this tool. It is still counted for balancing.
	AnnotationIgnore = "harvester-antiaffinity.jaevans.github.io/ignore"
	// LabelManagedBy marks migrations created by this tool.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "harvester-antiaffinity"
)

// NewScheme returns a scheme with core and KubeVirt types registered.
func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = kubevirtv1.AddToScheme(s)
	return s
}

// Skip records a member that was not considered and why.
type Skip struct {
	Namespace, Name, Reason string
}

// State is a snapshot of everything the planner needs.
type State struct {
	Nodes []planner.Node
	VMs   []planner.VM
	// Skipped members were not found or are not running on this cluster.
	Skipped []Skip
	// Unmovable maps namespace/name to why a running member cannot be migrated.
	Unmovable map[string]string
	// Busy explains why no migrations should be started at all, if non-empty.
	Busy string
}

// Collector reads Harvester state.
type Collector struct {
	Client client.Client
}

// Collect builds the planner input for the given members. Members whose VM
// does not exist on this Harvester cluster are reported in Skipped.
func (c *Collector) Collect(ctx context.Context, members []inventory.Member) (*State, error) {
	st := &State{Unmovable: map[string]string{}}

	busy, err := c.upgradeInProgress(ctx)
	if err != nil {
		return nil, err
	}
	st.Busy = busy

	var nodeList corev1.NodeList
	if err := c.Client.List(ctx, &nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	var podList corev1.PodList
	if err := c.Client.List(ctx, &podList); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	var vmiList kubevirtv1.VirtualMachineInstanceList
	if err := c.Client.List(ctx, &vmiList); err != nil {
		return nil, fmt.Errorf("listing virtualmachineinstances: %w", err)
	}
	var vmList kubevirtv1.VirtualMachineList
	if err := c.Client.List(ctx, &vmList); err != nil {
		return nil, fmt.Errorf("listing virtualmachines: %w", err)
	}
	var vmimList kubevirtv1.VirtualMachineInstanceMigrationList
	if err := c.Client.List(ctx, &vmimList); err != nil {
		return nil, fmt.Errorf("listing virtualmachineinstancemigrations: %w", err)
	}

	eligible := make([]*corev1.Node, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		if nodeEligible(&nodeList.Items[i]) {
			eligible = append(eligible, &nodeList.Items[i])
		}
	}
	free := freeMemory(eligible, podList.Items)
	for _, n := range eligible {
		st.Nodes = append(st.Nodes, planner.Node{Name: n.Name, FreeMemory: free[n.Name]})
	}

	podsByUID := make(map[string]*corev1.Pod, len(podList.Items))
	for i := range podList.Items {
		podsByUID[string(podList.Items[i].UID)] = &podList.Items[i]
	}
	vms := make(map[string]*kubevirtv1.VirtualMachine, len(vmList.Items))
	for i := range vmList.Items {
		vms[key(vmList.Items[i].Namespace, vmList.Items[i].Name)] = &vmList.Items[i]
	}
	vmis := make(map[string]*kubevirtv1.VirtualMachineInstance, len(vmiList.Items))
	for i := range vmiList.Items {
		vmis[key(vmiList.Items[i].Namespace, vmiList.Items[i].Name)] = &vmiList.Items[i]
	}
	// Any unfinished migration, whoever created it, pauses the whole pass:
	// a maintenance evacuation, a KubeVirt workload update or a user's
	// migration may be moving a sibling of a VM we would pick, and the planner
	// would see that sibling at its source node.
	migrating := map[string]bool{}
	for _, m := range vmimList.Items {
		if !migrationFinished(&m) {
			migrating[key(m.Namespace, m.Spec.VMIName)] = true
			if st.Busy == "" {
				st.Busy = fmt.Sprintf("migration %s/%s of VMI %s is still in progress", m.Namespace, m.Name, m.Spec.VMIName)
			}
		}
	}
	for _, n := range nodeList.Items {
		if v := n.Annotations[AnnotationMaintainStatus]; v == "running" && st.Busy == "" {
			st.Busy = fmt.Sprintf("node %s is entering maintenance mode", n.Name)
		}
	}

	for _, m := range members {
		k := key(m.Namespace, m.Name)
		vm, ok := vms[k]
		if !ok {
			st.Skipped = append(st.Skipped, Skip{m.Namespace, m.Name, "no such VirtualMachine on this cluster"})
			continue
		}
		if m.UID == "" {
			st.Skipped = append(st.Skipped, Skip{m.Namespace, m.Name, "no providerID to confirm the VM identity"})
			continue
		}
		if string(vm.UID) != m.UID {
			st.Skipped = append(st.Skipped, Skip{m.Namespace, m.Name, fmt.Sprintf("VirtualMachine UID %s does not match rancher providerID %s", vm.UID, m.UID)})
			continue
		}
		vmi, ok := vmis[k]
		if !ok || vmi.Status.NodeName == "" {
			st.Skipped = append(st.Skipped, Skip{m.Namespace, m.Name, "VM is not running"})
			continue
		}
		reason := movable(vm, vmi, migrating[k])
		if reason != "" {
			st.Unmovable[k] = reason
		}
		st.VMs = append(st.VMs, planner.VM{
			Namespace:     m.Namespace,
			Name:          m.Name,
			Group:         m.Group(),
			Priority:      m.Priority(),
			Node:          vmi.Status.NodeName,
			Movable:       reason == "",
			MemoryRequest: launcherMemory(vmi, podsByUID),
			AllowedNodes:  allowedNodes(vmi, podsByUID, eligible),
		})
	}
	sort.Slice(st.VMs, func(i, j int) bool { return st.VMs[i].Key() < st.VMs[j].Key() })
	return st, nil
}

// movable returns why a VM cannot be migrated right now, or "" if it can.
func movable(vm *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance, migrating bool) string {
	switch {
	case vm.Annotations[AnnotationIgnore] == "true" || vmi.Annotations[AnnotationIgnore] == "true":
		return "opted out via " + AnnotationIgnore
	case vmi.DeletionTimestamp != nil:
		return "VMI is being deleted"
	case vmi.Status.Phase != kubevirtv1.Running:
		return fmt.Sprintf("VMI phase is %s", vmi.Status.Phase)
	case !conditionTrue(vmi, kubevirtv1.VirtualMachineInstanceIsMigratable):
		return "VMI is not live-migratable"
	case !conditionTrue(vmi, kubevirtv1.VirtualMachineInstanceReady):
		return "VMI is not ready"
	case migrating:
		return "a migration is already in progress"
	case vmi.Status.MigrationState != nil && !vmi.Status.MigrationState.Completed && !vmi.Status.MigrationState.Failed:
		return "a migration is already in progress"
	}
	return ""
}

func conditionTrue(vmi *kubevirtv1.VirtualMachineInstance, t kubevirtv1.VirtualMachineInstanceConditionType) bool {
	for _, c := range vmi.Status.Conditions {
		if c.Type == t {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// migrationFinished deliberately does not treat a migration being deleted as
// finished: an abort can be slow or fail and the migration still complete.
func migrationFinished(m *kubevirtv1.VirtualMachineInstanceMigration) bool {
	return m.Status.Phase == kubevirtv1.MigrationSucceeded ||
		m.Status.Phase == kubevirtv1.MigrationFailed
}

// nodeEligible reports whether a node may receive migrated VMs at all.
func nodeEligible(n *corev1.Node) bool {
	if n.Spec.Unschedulable || n.DeletionTimestamp != nil {
		return false
	}
	if _, ok := n.Annotations[AnnotationMaintainStatus]; ok {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// allowedNodes filters eligible nodes by required node affinity, node
// selector and tolerations. The running launcher pod is used as the template
// because KubeVirt adds constraints there that are not in the VMI spec
// (kubevirt.io/schedulable, host CPU model labels); the target pod gets the
// same ones. The VMI spec is the fallback when the pod cannot be found.
func allowedNodes(vmi *kubevirtv1.VirtualMachineInstance, pods map[string]*corev1.Pod, eligible []*corev1.Node) []string {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		NodeSelector: vmi.Spec.NodeSelector,
		Affinity:     vmi.Spec.Affinity,
		Tolerations:  vmi.Spec.Tolerations,
	}}
	if p := launcherPod(vmi, pods); p != nil {
		pod = p
	}
	required := nodeaffinity.GetRequiredNodeAffinity(pod)
	var out []string
	for _, n := range eligible {
		if ok, err := required.Match(n); err != nil || !ok {
			continue
		}
		if _, untolerated := v1helper.FindMatchingUntoleratedTaint(klog.Background(), n.Spec.Taints, pod.Spec.Tolerations, func(t *corev1.Taint) bool {
			return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
		}, false); untolerated {
			continue
		}
		out = append(out, n.Name)
	}
	return out
}

// freeMemory returns allocatable memory minus requests of non-terminal pods,
// per node.
func freeMemory(nodes []*corev1.Node, pods []corev1.Pod) map[string]int64 {
	free := make(map[string]int64, len(nodes))
	for _, n := range nodes {
		free[n.Name] = n.Status.Allocatable.Memory().Value()
	}
	for i := range pods {
		p := &pods[i]
		if _, ok := free[p.Spec.NodeName]; !ok || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		free[p.Spec.NodeName] -= podMemoryRequest(p)
	}
	return free
}

func podMemoryRequest(p *corev1.Pod) int64 {
	var total int64
	for _, c := range p.Spec.Containers {
		total += c.Resources.Requests.Memory().Value()
	}
	var initMax int64
	for _, c := range p.Spec.InitContainers {
		initMax = max(initMax, c.Resources.Requests.Memory().Value())
	}
	total = max(total, initMax)
	if p.Spec.Overhead != nil {
		total += p.Spec.Overhead.Memory().Value()
	}
	return total
}

// launcherMemory is the memory the migration target pod will request. The
// running virt-launcher pod is the best estimate; the VMI spec is a fallback.
func launcherMemory(vmi *kubevirtv1.VirtualMachineInstance, pods map[string]*corev1.Pod) int64 {
	if p := launcherPod(vmi, pods); p != nil {
		return podMemoryRequest(p)
	}
	if r, ok := vmi.Spec.Domain.Resources.Requests[corev1.ResourceMemory]; ok {
		return r.Value()
	}
	if vmi.Spec.Domain.Memory != nil && vmi.Spec.Domain.Memory.Guest != nil {
		return vmi.Spec.Domain.Memory.Guest.Value()
	}
	return 0
}

func launcherPod(vmi *kubevirtv1.VirtualMachineInstance, pods map[string]*corev1.Pod) *corev1.Pod {
	for uid, node := range vmi.Status.ActivePods {
		if p, ok := pods[string(uid)]; ok && node == vmi.Status.NodeName {
			return p
		}
	}
	return nil
}

func (c *Collector) upgradeInProgress(ctx context.Context) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("harvesterhci.io/v1beta1")
	list.SetKind("UpgradeList")
	if err := c.Client.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("listing harvester upgrades: %w", err)
	}
	for _, u := range list.Items {
		state := u.GetLabels()[LabelUpgradeState]
		if state != "Succeeded" && state != "Failed" && !upgradeCompleted(&u) {
			return fmt.Sprintf("harvester upgrade %s/%s is in progress (state %q)", u.GetNamespace(), u.GetName(), state), nil
		}
	}
	return "", nil
}

// upgradeCompleted reports whether the Completed condition has settled to
// True or False; Harvester leaves it Unknown or absent while running.
func upgradeCompleted(u *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Completed" {
			return m["status"] == "True" || m["status"] == "False"
		}
	}
	return false
}

func key(ns, name string) string { return ns + "/" + name }
