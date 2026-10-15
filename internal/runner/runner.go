// Package runner ties inventory, state collection, planning and migration
// into a single balancing pass.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jaevans/harvester-antiaffinity/internal/harvester"
	"github.com/jaevans/harvester-antiaffinity/internal/inventory"
	"github.com/jaevans/harvester-antiaffinity/internal/planner"
)

// Collector abstracts harvester.Collector for tests.
type Collector interface {
	Collect(ctx context.Context, members []inventory.Member) (*harvester.State, error)
}

// Migrator abstracts harvester.Migrator for tests.
type Migrator interface {
	Migrate(ctx context.Context, move planner.Move) error
}

// Runner performs balancing passes.
type Runner struct {
	Source    inventory.Source
	Collector Collector
	Migrator  Migrator
	Log       *slog.Logger
	// MaxMigrations caps migrations per pass; 0 means unlimited.
	MaxMigrations int
	// MaxFailures stops the pass after this many failed migrations.
	MaxFailures int
	DryRun      bool
}

// Result summarises a pass.
type Result struct {
	Planned   []planner.Move
	Succeeded []planner.Move
	Failed    []planner.Move
	// Skipped is set when the pass did not run, e.g. during an upgrade.
	Skipped string
}

// RunOnce performs one balancing pass. Cluster state is re-read and the plan
// recomputed after every migration, so each migration is decided on fresh
// data and at most one migration is in flight at a time.
func (r *Runner) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	members, err := r.Source.Members(ctx)
	if err != nil {
		return res, err
	}
	r.Log.Info("discovered group members", "count", len(members))

	failed := map[string]bool{}
	for iteration := 0; ; iteration++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Every successful move strictly improves balance and every failed VM
		// is excluded, so this is a backstop against bugs, not a normal exit.
		if iteration > 2*len(members)+10 {
			return res, fmt.Errorf("giving up after %d iterations without converging", iteration)
		}
		st, err := r.Collector.Collect(ctx, members)
		if err != nil {
			return res, err
		}
		if iteration == 0 {
			r.logState(st)
		}
		if st.Busy != "" {
			res.Skipped = st.Busy
			r.Log.Warn("not migrating", "reason", st.Busy)
			return res, nil
		}
		for i := range st.VMs {
			if failed[st.VMs[i].Key()] {
				st.VMs[i].Movable = false
			}
		}

		remaining := 0
		if r.MaxMigrations > 0 {
			remaining = r.MaxMigrations - len(res.Succeeded) - len(res.Failed)
			if remaining <= 0 {
				r.Log.Info("reached migration limit for this run", "limit", r.MaxMigrations)
				return res, nil
			}
		}
		plan := planner.Plan(st.Nodes, st.VMs, planner.Options{MaxMoves: remaining})
		if iteration == 0 {
			res.Planned = plan
			for _, m := range plan {
				r.Log.Info("planned migration", "vm", m.Namespace+"/"+m.Name, "group", m.Group, "from", m.From, "to", m.To)
			}
		}
		if len(plan) == 0 {
			r.Log.Info("all groups are balanced")
			return res, nil
		}
		if r.DryRun {
			r.Log.Info("dry run: not migrating", "planned", len(plan))
			return res, nil
		}

		move := plan[0]
		log := r.Log.With("vm", move.Namespace+"/"+move.Name, "group", move.Group, "from", move.From, "to", move.To)
		log.Info("starting live migration")
		if err := r.Migrator.Migrate(ctx, move); err != nil {
			log.Error("live migration failed", "error", err)
			res.Failed = append(res.Failed, move)
			failed[move.Namespace+"/"+move.Name] = true
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			if r.MaxFailures > 0 && len(res.Failed) >= r.MaxFailures {
				return res, fmt.Errorf("stopping after %d failed migrations", len(res.Failed))
			}
			continue
		}
		log.Info("live migration succeeded")
		res.Succeeded = append(res.Succeeded, move)
	}
}

func (r *Runner) logState(st *harvester.State) {
	nodes := make([]string, 0, len(st.Nodes))
	for _, n := range st.Nodes {
		nodes = append(nodes, n.Name)
	}
	r.Log.Info("eligible nodes", "nodes", nodes)
	for _, s := range st.Skipped {
		r.Log.Debug("member skipped", "vm", s.Namespace+"/"+s.Name, "reason", s.Reason)
	}
	keys := make([]string, 0, len(st.Unmovable))
	for k := range st.Unmovable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.Log.Info("VM cannot be migrated", "vm", k, "reason", st.Unmovable[k])
	}
	dist := planner.Distribution(st.VMs)
	groups := make([]string, 0, len(dist))
	for g := range dist {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		r.Log.Info("current placement", "group", g, "nodes", dist[g])
	}
}
