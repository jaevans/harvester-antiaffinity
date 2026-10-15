package planner

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

const gib = int64(1) << 30

func nodes(names ...string) []Node {
	out := make([]Node, 0, len(names))
	for _, n := range names {
		out = append(out, Node{Name: n, FreeMemory: 100 * gib})
	}
	return out
}

func vm(group, name, node string, allowed ...string) VM {
	return VM{
		Namespace:     "ns",
		Name:          name,
		Group:         group,
		Node:          node,
		Movable:       true,
		MemoryRequest: gib,
		AllowedNodes:  allowed,
	}
}

// apply returns the VMs after executing moves, failing on any move that does
// not start from the VM's current node.
func apply(t *testing.T, vms []VM, moves []Move) []VM {
	t.Helper()
	out := append([]VM(nil), vms...)
	for _, m := range moves {
		found := false
		for i := range out {
			if out[i].Namespace == m.Namespace && out[i].Name == m.Name {
				if out[i].Node != m.From {
					t.Fatalf("move %s starts from %s but VM is on %s", m, m.From, out[i].Node)
				}
				out[i].Node = m.To
				found = true
			}
		}
		if !found {
			t.Fatalf("move %s references unknown VM", m)
		}
	}
	return out
}

func spread(vms []VM, group string, nodeNames []string) (minC, maxC int) {
	counts := map[string]int{}
	for _, n := range nodeNames {
		counts[n] = 0
	}
	for _, v := range vms {
		if v.Group != group {
			continue
		}
		if _, ok := counts[v.Node]; ok {
			counts[v.Node]++
		}
	}
	minC, maxC = 1<<30, -1
	for _, c := range counts {
		minC = min(minC, c)
		maxC = max(maxC, c)
	}
	return minC, maxC
}

func TestPlanEtcdPoolOnTwoNodes(t *testing.T) {
	all := []string{"matrix", "oasis", "tron"}
	vms := []VM{
		vm("stl-west-1/control-plane", "cp-a", "oasis", all...),
		vm("stl-west-1/control-plane", "cp-b", "oasis", all...),
		vm("stl-west-1/control-plane", "cp-c", "matrix", all...),
	}
	moves := Plan(nodes(all...), vms, Options{})
	want := []Move{{Namespace: "ns", Name: "cp-a", Group: "stl-west-1/control-plane", From: "oasis", To: "tron"}}
	if !reflect.DeepEqual(moves, want) {
		t.Fatalf("got %v, want %v", moves, want)
	}
}

func TestPlanAlreadyBalancedIsNoop(t *testing.T) {
	all := []string{"a", "b", "c"}
	vms := []VM{
		vm("g", "1", "a", all...), vm("g", "2", "b", all...), vm("g", "3", "c", all...),
		vm("g", "4", "a", all...), vm("g", "5", "b", all...),
	}
	if moves := Plan(nodes(all...), vms, Options{}); len(moves) != 0 {
		t.Fatalf("expected no moves, got %v", moves)
	}
}

func TestPlanWorkerPoolSevenOnThree(t *testing.T) {
	// Real layout observed on stl-west-1 workers: matrix=3 oasis=3 tron=1.
	all := []string{"matrix", "oasis", "tron"}
	vms := []VM{
		vm("w", "bcldk", "matrix", all...), vm("w", "j66gd", "matrix", all...), vm("w", "shdrd", "matrix", all...),
		vm("w", "c4lqd", "tron", all...),
		vm("w", "djvrl", "oasis", all...), vm("w", "lpww9", "oasis", all...), vm("w", "nbr66", "oasis", all...),
	}
	moves := Plan(nodes(all...), vms, Options{})
	if len(moves) != 1 {
		t.Fatalf("expected exactly one move, got %v", moves)
	}
	if moves[0].To != "tron" {
		t.Fatalf("expected move to tron, got %v", moves[0])
	}
	if lo, hi := spread(apply(t, vms, moves), "w", all); hi-lo > 1 {
		t.Fatalf("still unbalanced: min=%d max=%d", lo, hi)
	}
}

func TestPlanAllOnOneNode(t *testing.T) {
	all := []string{"a", "b", "c", "d"}
	var vms []VM
	for i := range 6 {
		vms = append(vms, vm("g", fmt.Sprintf("vm-%d", i), "a", all...))
	}
	moves := Plan(nodes(all...), vms, Options{})
	if len(moves) != 4 {
		t.Fatalf("expected 4 moves for 6 VMs on 4 nodes, got %d: %v", len(moves), moves)
	}
	if lo, hi := spread(apply(t, vms, moves), "g", all); hi-lo > 1 {
		t.Fatalf("still unbalanced: min=%d max=%d", lo, hi)
	}
}

func TestPlanRespectsMovable(t *testing.T) {
	all := []string{"a", "b"}
	pinned := vm("g", "1", "a", all...)
	pinned.Movable = false
	vms := []VM{pinned, vm("g", "2", "a", all...)}
	moves := Plan(nodes(all...), vms, Options{})
	if len(moves) != 1 || moves[0].Name != "2" {
		t.Fatalf("expected only the movable VM to move, got %v", moves)
	}

	other := vm("g", "2", "a", all...)
	other.Movable = false
	if moves := Plan(nodes(all...), []VM{pinned, other}, Options{}); len(moves) != 0 {
		t.Fatalf("expected no moves when nothing is movable, got %v", moves)
	}
}

func TestPlanRespectsAllowedNodes(t *testing.T) {
	all := []string{"a", "b", "c"}
	vms := []VM{
		vm("g", "1", "a", "a", "b"),
		vm("g", "2", "a", "a", "b"),
	}
	moves := Plan(nodes(all...), vms, Options{})
	if len(moves) != 1 || moves[0].To != "b" {
		t.Fatalf("expected one move to b (c is not allowed), got %v", moves)
	}
}

func TestPlanRespectsFreeMemory(t *testing.T) {
	all := []string{"a", "b", "c"}
	ns := nodes(all...)
	ns[1].FreeMemory = gib / 2 // b cannot fit a VM
	vms := []VM{vm("g", "1", "a", all...), vm("g", "2", "a", all...)}
	moves := Plan(ns, vms, Options{})
	if len(moves) != 1 || moves[0].To != "c" {
		t.Fatalf("expected move to c, got %v", moves)
	}

	ns[2].FreeMemory = 0
	if moves := Plan(ns, vms, Options{}); len(moves) != 0 {
		t.Fatalf("expected no moves when no node fits, got %v", moves)
	}
}

func TestPlanMemoryIsSharedAcrossGroups(t *testing.T) {
	all := []string{"a", "b"}
	ns := nodes(all...)
	ns[1].FreeMemory = gib // room for exactly one more VM
	vms := []VM{
		vm("g1", "1", "a", all...), vm("g1", "2", "a", all...),
		vm("g2", "3", "a", all...), vm("g2", "4", "a", all...),
	}
	moves := Plan(ns, vms, Options{})
	if len(moves) != 1 || moves[0].Group != "g1" {
		t.Fatalf("expected a single move for g1 then no room for g2, got %v", moves)
	}
}

func TestPlanPrefersEmptierAndFreerTarget(t *testing.T) {
	all := []string{"a", "b", "c"}
	ns := nodes(all...)
	ns[2].FreeMemory = 200 * gib
	vms := []VM{vm("g", "1", "a", all...), vm("g", "2", "a", all...)}
	moves := Plan(ns, vms, Options{})
	if len(moves) != 1 || moves[0].To != "c" {
		t.Fatalf("expected tie broken by free memory to c, got %v", moves)
	}
}

func TestPlanIgnoresVMsOnIneligibleNodes(t *testing.T) {
	// "cordoned" is not in the node list: its VMs are neither counted nor moved.
	all := []string{"a", "b"}
	vms := []VM{
		vm("g", "1", "cordoned", all...),
		vm("g", "2", "cordoned", all...),
		vm("g", "3", "a", all...),
	}
	if moves := Plan(nodes(all...), vms, Options{}); len(moves) != 0 {
		t.Fatalf("expected no moves, got %v", moves)
	}
}

func TestPlanGroupPriorityAndMaxMoves(t *testing.T) {
	all := []string{"a", "b"}
	workers := []VM{vm("c/workers", "w1", "a", all...), vm("c/workers", "w2", "a", all...)}
	etcd := []VM{vm("c/etcd", "e1", "a", all...), vm("c/etcd", "e2", "a", all...)}
	for i := range workers {
		workers[i].Priority = 2
	}
	vms := append(workers, etcd...)
	moves := Plan(nodes(all...), vms, Options{MaxMoves: 1})
	if len(moves) != 1 || moves[0].Group != "c/etcd" {
		t.Fatalf("expected the etcd group to be balanced first, got %v", moves)
	}
}

func TestPlanNeverMovesSameVMTwice(t *testing.T) {
	all := []string{"a", "b", "c"}
	vms := []VM{vm("g", "1", "a", all...), vm("g", "2", "a", all...), vm("g", "3", "a", all...)}
	moves := Plan(nodes(all...), vms, Options{})
	seen := map[string]bool{}
	for _, m := range moves {
		if seen[m.Name] {
			t.Fatalf("VM %s moved twice: %v", m.Name, moves)
		}
		seen[m.Name] = true
	}
	if len(moves) != 2 {
		t.Fatalf("expected 2 moves, got %v", moves)
	}
}

func TestPlanDoesNotMutateInput(t *testing.T) {
	all := []string{"a", "b"}
	vms := []VM{vm("g", "1", "a", all...), vm("g", "2", "a", all...)}
	before := append([]VM(nil), vms...)
	Plan(nodes(all...), vms, Options{})
	if !reflect.DeepEqual(before, vms) {
		t.Fatalf("Plan mutated its input")
	}
}

// TestPlanRandomizedConverges checks the core invariant on random inputs:
// with unconstrained placement, after applying the plan every group's
// per-node counts differ by at most one, and no move is a no-op.
func TestPlanRandomizedConverges(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for iter := range 500 {
		nNodes := 2 + r.Intn(5)
		var names []string
		for i := range nNodes {
			names = append(names, fmt.Sprintf("n%d", i))
		}
		var vms []VM
		for g := range 1 + r.Intn(4) {
			for i := range 1 + r.Intn(9) {
				vms = append(vms, vm(fmt.Sprintf("g%d", g), fmt.Sprintf("g%d-vm%d", g, i), names[r.Intn(nNodes)], names...))
			}
		}
		moves := Plan(nodes(names...), vms, Options{})
		for _, m := range moves {
			if m.From == m.To {
				t.Fatalf("iter %d: no-op move %v", iter, m)
			}
		}
		after := apply(t, vms, moves)
		for _, g := range []string{"g0", "g1", "g2", "g3"} {
			if lo, hi := spread(after, g, names); hi >= 0 && hi-lo > 1 {
				t.Fatalf("iter %d: group %s unbalanced (min=%d max=%d) after %v", iter, g, lo, hi, moves)
			}
		}
	}
}

func TestDistribution(t *testing.T) {
	vms := []VM{vm("g", "1", "b"), vm("g", "2", "a"), vm("g", "3", "b"), vm("h", "4", "c")}
	got := Distribution(vms)
	want := map[string]string{"g": "a=1 b=2", "h": "c=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
