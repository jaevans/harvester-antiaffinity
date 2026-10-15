// Package inventory discovers which Harvester VMs belong together, i.e. are
// members of the same downstream cluster machine pool.
package inventory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	RoleEtcd         = "etcd"
	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"

	LabelClusterName  = "rke.cattle.io/cluster-name"
	LabelPoolName     = "rke.cattle.io/rke-machine-pool-name"
	LabelDeployment   = "cluster.x-k8s.io/deployment-name"
	LabelEtcdRole     = "rke.cattle.io/etcd-role"
	LabelCPRole       = "rke.cattle.io/control-plane-role"
	LabelWorkerRole   = "rke.cattle.io/worker-role"
	LabelMachineSet   = "harvesterhci.io/machineSetName"
	LabelGuestCluster = "guestcluster.harvesterhci.io/name"

	providerIDPrefix = "harvester://"
)

// HarvesterMachineGVR is the Rancher node-driver machine resource. Its name
// equals the Harvester VM name and spec.vmNamespace holds the VM namespace.
var HarvesterMachineGVR = schema.GroupVersionResource{Group: "rke-machine.cattle.io", Version: "v1", Resource: "harvestermachines"}

// VirtualMachineGVR is the KubeVirt VM resource on Harvester.
var VirtualMachineGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}

// Member is one VM that belongs to a group.
type Member struct {
	Cluster   string
	Pool      string
	Namespace string
	Name      string
	// UID is the expected Harvester VirtualMachine UID, if known. It guards
	// against matching a same-named VM on a different Harvester cluster.
	UID   string
	Roles []string
}

// Group is the key VMs are balanced by.
func (m Member) Group() string { return m.Cluster + "/" + m.Pool }

// Priority orders groups for balancing: etcd first, then control plane, then
// everything else, since losing etcd quorum is the costliest failure.
func (m Member) Priority() int {
	switch {
	case slices.Contains(m.Roles, RoleEtcd):
		return 0
	case slices.Contains(m.Roles, RoleControlPlane):
		return 1
	default:
		return 2
	}
}

// Source lists group members.
type Source interface {
	Members(ctx context.Context) ([]Member, error)
}

// Filter restricts which downstream clusters are considered.
type Filter struct {
	Include []string
	Exclude []string
}

func (f Filter) allows(cluster string) bool {
	if slices.Contains(f.Exclude, cluster) {
		return false
	}
	return len(f.Include) == 0 || slices.Contains(f.Include, cluster)
}

// RancherSource reads HarvesterMachine objects from the Rancher management
// ("local") cluster.
type RancherSource struct {
	Client dynamic.Interface
	// Namespace holding provisioning objects; Rancher uses fleet-default. Empty
	// means all namespaces.
	Namespace string
	Filter    Filter
}

func (s *RancherSource) Members(ctx context.Context) ([]Member, error) {
	list, err := s.Client.Resource(HarvesterMachineGVR).Namespace(s.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing harvestermachines from rancher: %w", err)
	}
	var out []Member
	for i := range list.Items {
		m, ok := memberFromHarvesterMachine(&list.Items[i])
		if ok && s.Filter.allows(m.Cluster) {
			out = append(out, m)
		}
	}
	sortMembers(out)
	return out, nil
}

func memberFromHarvesterMachine(u *unstructured.Unstructured) (Member, bool) {
	if u.GetDeletionTimestamp() != nil {
		return Member{}, false
	}
	labels := u.GetLabels()
	cluster := labels[LabelClusterName]
	pool := labels[LabelPoolName]
	if pool == "" {
		pool = labels[LabelDeployment]
	}
	vmNamespace, _, _ := unstructured.NestedString(u.Object, "spec", "vmNamespace")
	if cluster == "" || pool == "" || vmNamespace == "" {
		return Member{}, false
	}
	providerID, _, _ := unstructured.NestedString(u.Object, "spec", "providerID")
	if providerID == "" {
		providerID, _, _ = unstructured.NestedString(u.Object, "status", "providerID")
	}
	var roles []string
	for label, role := range map[string]string{LabelEtcdRole: RoleEtcd, LabelCPRole: RoleControlPlane, LabelWorkerRole: RoleWorker} {
		if labels[label] == "true" {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return Member{
		Cluster:   cluster,
		Pool:      pool,
		Namespace: vmNamespace,
		Name:      u.GetName(),
		UID:       strings.TrimPrefix(providerID, providerIDPrefix),
		Roles:     roles,
	}, true
}

// LabelSource groups VMs on Harvester by the labels the Harvester node driver
// puts on every VM it creates, without contacting Rancher. Roles are unknown,
// so all groups share the same priority.
type LabelSource struct {
	Client dynamic.Interface
	Filter Filter
}

func (s *LabelSource) Members(ctx context.Context) ([]Member, error) {
	list, err := s.Client.Resource(VirtualMachineGVR).List(ctx, metav1.ListOptions{LabelSelector: LabelMachineSet})
	if err != nil {
		return nil, fmt.Errorf("listing virtualmachines: %w", err)
	}
	var out []Member
	for _, vm := range list.Items {
		if vm.GetDeletionTimestamp() != nil {
			continue
		}
		labels := vm.GetLabels()
		cluster := labels[LabelGuestCluster]
		if !s.Filter.allows(cluster) {
			continue
		}
		out = append(out, Member{
			Cluster:   cluster,
			Pool:      labels[LabelMachineSet],
			Namespace: vm.GetNamespace(),
			Name:      vm.GetName(),
			UID:       string(vm.GetUID()),
		})
	}
	sortMembers(out)
	return out, nil
}

func sortMembers(ms []Member) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Namespace != ms[j].Namespace {
			return ms[i].Namespace < ms[j].Namespace
		}
		return ms[i].Name < ms[j].Name
	})
}
