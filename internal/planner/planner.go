// Package planner computes live migrations that spread the VMs of each group
// (a Rancher machine pool) as evenly as possible across Harvester nodes.
//
// The planner is pure: it works on a snapshot of nodes and VMs and never talks
// to a cluster, so every placement decision can be unit tested.
package planner

import (
	"fmt"
	"sort"
	"strings"
)

// Node is a Harvester node that may receive migrated VMs.
type Node struct {
	Name string
	// FreeMemory is allocatable memory minus the requests of pods already
	// scheduled on the node, in bytes.
	FreeMemory int64
}

// VM is one member of a group as currently placed on the cluster.
type VM struct {
	Namespace string
	Name      string
	Group     string
	// Priority orders groups: lower values are balanced first.
	Priority int
	// Node is the node the VM is running on.
	Node string
	// Movable is false when the VM must not be migrated right now (not
	// live-migratable, already migrating, opted out, ...).
	Movable bool
	// MemoryRequest is the memory the VM's launcher pod requests, in bytes.
	MemoryRequest int64
	// AllowedNodes are the eligible nodes this VM could be scheduled on.
	AllowedNodes []string
}

// Key returns namespace/name.
func (v VM) Key() string { return v.Namespace + "/" + v.Name }

// Move is a single planned live migration.
type Move struct {
	Namespace string
	Name      string
	Group     string
	From      string
	To        string
}

func (m Move) String() string {
	return fmt.Sprintf("%s/%s (%s): %s -> %s", m.Namespace, m.Name, m.Group, m.From, m.To)
}

// Options tunes planning.
type Options struct {
	// MaxMoves caps the number of moves returned; 0 means unlimited.
	MaxMoves int
}

// Plan returns the moves needed so that, within every group, the number of
// VMs per eligible node differs by at most one.
//
// Only nodes listed in nodes take part in balancing. A VM running on a node
// that is not listed (cordoned, in maintenance, not ready) is left where it is
// and is not counted, because it cannot be balanced against and Harvester is
// expected to move it anyway.
//
// Groups are processed in ascending Priority then name, and memory consumed by
// earlier moves is accounted for in later ones.
func Plan(nodes []Node, vms []VM, opts Options) []Move {
	free := make(map[string]int64, len(nodes))
	for _, n := range nodes {
		free[n.Name] = n.FreeMemory
	}

	groups := map[string][]VM{}
	prio := map[string]int{}
	for _, vm := range vms {
		if _, ok := prio[vm.Group]; !ok || vm.Priority < prio[vm.Group] {
			prio[vm.Group] = vm.Priority
		}
		groups[vm.Group] = append(groups[vm.Group], vm)
	}
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Slice(names, func(i, j int) bool {
		if prio[names[i]] != prio[names[j]] {
			return prio[names[i]] < prio[names[j]]
		}
		return names[i] < names[j]
	})

	var moves []Move
	for _, g := range names {
		for _, m := range planGroup(nodes, groups[g], free) {
			if opts.MaxMoves > 0 && len(moves) >= opts.MaxMoves {
				return moves
			}
			moves = append(moves, m)
		}
	}
	return moves
}

// planGroup greedily moves a VM from a fullest node to an emptiest allowed
// node while the gap is at least two. Each such move strictly decreases the
// sum of squared per-node counts, so the loop terminates.
func planGroup(nodes []Node, vms []VM, free map[string]int64) []Move {
	counts := make(map[string]int, len(nodes))
	for _, n := range nodes {
		counts[n.Name] = 0
	}
	members := make([]*VM, 0, len(vms))
	for i := range vms {
		vm := vms[i]
		if _, eligible := counts[vm.Node]; !eligible {
			continue
		}
		counts[vm.Node]++
		members = append(members, &vm)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Key() < members[j].Key() })

	moved := map[string]bool{}
	var moves []Move
	for {
		m, vm := bestMove(nodes, members, counts, free, moved)
		if vm == nil {
			return moves
		}
		counts[m.From]--
		counts[m.To]++
		free[m.From] += vm.MemoryRequest
		free[m.To] -= vm.MemoryRequest
		vm.Node = m.To
		moved[vm.Key()] = true
		moves = append(moves, m)
	}
}

func bestMove(nodes []Node, members []*VM, counts map[string]int, free map[string]int64, moved map[string]bool) (Move, *VM) {
	sources := make([]string, 0, len(nodes))
	for _, n := range nodes {
		sources = append(sources, n.Name)
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if counts[sources[i]] != counts[sources[j]] {
			return counts[sources[i]] > counts[sources[j]]
		}
		return sources[i] < sources[j]
	})

	for _, src := range sources {
		c := counts[src]
		if c < 2 {
			break
		}
		for _, vm := range members {
			if vm.Node != src || !vm.Movable || moved[vm.Key()] {
				continue
			}
			if to := bestTarget(vm, c, counts, free); to != "" {
				return Move{Namespace: vm.Namespace, Name: vm.Name, Group: vm.Group, From: src, To: to}, vm
			}
		}
	}
	return Move{}, nil
}

func bestTarget(vm *VM, srcCount int, counts map[string]int, free map[string]int64) string {
	best := ""
	for _, t := range vm.AllowedNodes {
		tc, eligible := counts[t]
		if !eligible || t == vm.Node || tc > srcCount-2 || free[t] < vm.MemoryRequest {
			continue
		}
		if best == "" ||
			tc < counts[best] ||
			(tc == counts[best] && free[t] > free[best]) ||
			(tc == counts[best] && free[t] == free[best] && t < best) {
			best = t
		}
	}
	return best
}

// Distribution renders per-node counts for each group, e.g.
// "c1/pool: node-a=2 node-b=1", sorted for stable logs.
func Distribution(vms []VM) map[string]string {
	per := map[string]map[string]int{}
	for _, vm := range vms {
		if per[vm.Group] == nil {
			per[vm.Group] = map[string]int{}
		}
		per[vm.Group][vm.Node]++
	}
	out := make(map[string]string, len(per))
	for g, counts := range per {
		nodes := make([]string, 0, len(counts))
		for n := range counts {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		parts := make([]string, 0, len(nodes))
		for _, n := range nodes {
			parts = append(parts, fmt.Sprintf("%s=%d", n, counts[n]))
		}
		out[g] = strings.Join(parts, " ")
	}
	return out
}
