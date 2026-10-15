package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jaevans/harvester-antiaffinity/internal/harvester"
	"github.com/jaevans/harvester-antiaffinity/internal/inventory"
	"github.com/jaevans/harvester-antiaffinity/internal/planner"
)

type staticSource []inventory.Member

func (s staticSource) Members(context.Context) ([]inventory.Member, error) { return s, nil }

// world is an in-memory cluster: Collect reports VM placement and Migrate
// moves VMs, unless the VM is listed in fail.
type world struct {
	nodes      []planner.Node
	placement  map[string]string
	busy       string
	fail       map[string]bool
	migrations []planner.Move
	collects   int
}

func (w *world) Collect(_ context.Context, members []inventory.Member) (*harvester.State, error) {
	w.collects++
	st := &harvester.State{Nodes: w.nodes, Busy: w.busy}
	all := make([]string, 0, len(w.nodes))
	for _, n := range w.nodes {
		all = append(all, n.Name)
	}
	for _, m := range members {
		st.VMs = append(st.VMs, planner.VM{
			Namespace: m.Namespace, Name: m.Name, Group: m.Group(), Priority: m.Priority(),
			Node: w.placement[m.Name], Movable: true, MemoryRequest: 1, AllowedNodes: all,
		})
	}
	return st, nil
}

func (w *world) Migrate(_ context.Context, m planner.Move) error {
	w.migrations = append(w.migrations, m)
	if w.fail[m.Name] {
		return errors.New("boom")
	}
	w.placement[m.Name] = m.To
	return nil
}

func newWorld(placement map[string]string) *world {
	return &world{
		nodes:     []planner.Node{{Name: "a", FreeMemory: 100}, {Name: "b", FreeMemory: 100}, {Name: "c", FreeMemory: 100}},
		placement: placement,
		fail:      map[string]bool{},
	}
}

func members(names ...string) staticSource {
	var out staticSource
	for _, n := range names {
		out = append(out, inventory.Member{Cluster: "c", Pool: "p", Namespace: "ns", Name: n, Roles: []string{inventory.RoleEtcd}})
	}
	return out
}

func newRunner(w *world, src inventory.Source) *Runner {
	return &Runner{Source: src, Collector: w, Migrator: w, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestRunOnceBalances(t *testing.T) {
	w := newWorld(map[string]string{"1": "a", "2": "a", "3": "a"})
	res, err := newRunner(w, members("1", "2", "3")).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Succeeded) != 2 || len(res.Failed) != 0 {
		t.Fatalf("unexpected result %+v", res)
	}
	seen := map[string]bool{}
	for _, n := range w.placement {
		seen[n] = true
	}
	if len(seen) != 3 {
		t.Fatalf("VMs not spread: %v", w.placement)
	}
	if w.collects != 3 {
		t.Fatalf("expected state to be re-read after every migration (3 collects), got %d", w.collects)
	}
}

func TestRunOnceDryRun(t *testing.T) {
	w := newWorld(map[string]string{"1": "a", "2": "a"})
	r := newRunner(w, members("1", "2"))
	r.DryRun = true
	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Planned) != 1 || len(w.migrations) != 0 {
		t.Fatalf("dry run must plan but not migrate: %+v, migrations %v", res, w.migrations)
	}
}

func TestRunOnceBusySkips(t *testing.T) {
	w := newWorld(map[string]string{"1": "a", "2": "a"})
	w.busy = "upgrade running"
	res, err := newRunner(w, members("1", "2")).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != "upgrade running" || len(w.migrations) != 0 {
		t.Fatalf("expected skip without migrating, got %+v", res)
	}
}

func TestRunOnceMaxMigrations(t *testing.T) {
	w := newWorld(map[string]string{"1": "a", "2": "a", "3": "a"})
	r := newRunner(w, members("1", "2", "3"))
	r.MaxMigrations = 1
	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(w.migrations) != 1 || len(res.Succeeded) != 1 {
		t.Fatalf("expected exactly one migration, got %v", w.migrations)
	}
}

func TestRunOnceFailedVMIsNotRetried(t *testing.T) {
	w := newWorld(map[string]string{"1": "a", "2": "a"})
	w.fail["1"] = true
	res, err := newRunner(w, members("1", "2")).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 1 || len(res.Succeeded) != 1 || res.Succeeded[0].Name != "2" {
		t.Fatalf("expected VM 1 to fail once and VM 2 to be moved instead, got %+v", res)
	}
	if len(w.migrations) != 2 {
		t.Fatalf("expected 2 attempts, got %v", w.migrations)
	}
}

func TestRunOnceMaxFailures(t *testing.T) {
	w := newWorld(map[string]string{"1": "a", "2": "a", "3": "a"})
	w.fail = map[string]bool{"1": true, "2": true, "3": true}
	r := newRunner(w, members("1", "2", "3"))
	r.MaxFailures = 2
	res, err := r.RunOnce(context.Background())
	if err == nil {
		t.Fatalf("expected error after max failures")
	}
	if len(res.Failed) != 2 {
		t.Fatalf("expected to stop after 2 failures, got %+v", res)
	}
}

func TestRunOnceContextCancelled(t *testing.T) {
	w := newWorld(map[string]string{"1": "a", "2": "a"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newRunner(w, members("1", "2")).RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(w.migrations) != 0 {
		t.Fatalf("no migration should start after cancel")
	}
}
