package harvester

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jaevans/harvester-antiaffinity/internal/planner"
)

// Migrator executes a planned move as a KubeVirt live migration.
type Migrator struct {
	Client       client.Client
	Timeout      time.Duration
	PollInterval time.Duration
}

// Migrate live-migrates the VM to move.To and blocks until the migration
// finishes, fails or times out. On timeout or context cancellation the
// migration object is deleted, which makes KubeVirt abort it.
//
// The target node is pinned with spec.addedNodeSelector on
// kubernetes.io/hostname, which is how Harvester's own "migrate to node"
// action does it. It can only narrow the VM's own constraints, never bypass
// them.
func (m *Migrator) Migrate(ctx context.Context, move planner.Move) error {
	var vmi kubevirtv1.VirtualMachineInstance
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: move.Namespace, Name: move.Name}, &vmi); err != nil {
		return fmt.Errorf("getting VMI: %w", err)
	}
	if vmi.Status.NodeName != move.From {
		return fmt.Errorf("VMI is on %q, not %q as planned", vmi.Status.NodeName, move.From)
	}

	vmim := &kubevirtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "antiaffinity-" + move.Name + "-",
			Namespace:    move.Namespace,
			Labels:       map[string]string{LabelManagedBy: ManagedByValue},
			Annotations: map[string]string{
				"harvester-antiaffinity.jaevans.github.io/group":  move.Group,
				"harvester-antiaffinity.jaevans.github.io/source": move.From,
				"harvester-antiaffinity.jaevans.github.io/target": move.To,
			},
		},
		Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
			VMIName:           move.Name,
			AddedNodeSelector: map[string]string{corev1.LabelHostname: move.To},
		},
	}
	if err := m.Client.Create(ctx, vmim); err != nil {
		return fmt.Errorf("creating migration: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()
	phase, err := m.wait(waitCtx, client.ObjectKeyFromObject(vmim))
	if err != nil {
		m.abort(vmim)
		return fmt.Errorf("migration %s/%s: %w", vmim.Namespace, vmim.Name, err)
	}
	if phase != kubevirtv1.MigrationSucceeded {
		return fmt.Errorf("migration %s/%s ended in phase %s", vmim.Namespace, vmim.Name, phase)
	}

	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: move.Namespace, Name: move.Name}, &vmi); err != nil {
		return fmt.Errorf("getting VMI after migration: %w", err)
	}
	if vmi.Status.NodeName != move.To {
		return fmt.Errorf("migration succeeded but VMI is on %q, not %q", vmi.Status.NodeName, move.To)
	}
	return nil
}

func (m *Migrator) wait(ctx context.Context, key client.ObjectKey) (kubevirtv1.VirtualMachineInstanceMigrationPhase, error) {
	interval := m.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastErr error
	for {
		var cur kubevirtv1.VirtualMachineInstanceMigration
		// Transient API errors are retried until the deadline rather than
		// abandoning a migration that is probably still progressing.
		if err := m.Client.Get(ctx, key, &cur); err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("migration object disappeared")
			}
			lastErr = err
		} else if migrationFinished(&cur) {
			return cur.Status.Phase, nil
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("timed out or cancelled (last error: %v): %w", lastErr, ctx.Err())
			}
			return "", fmt.Errorf("timed out or cancelled: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// abort deletes the migration with a fresh context, because the caller's may
// already be done.
func (m *Migrator) abort(vmim *kubevirtv1.VirtualMachineInstanceMigration) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = client.IgnoreNotFound(m.Client.Delete(ctx, vmim))
}
