package harvester

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/jaevans/harvester-antiaffinity/internal/planner"
)

// fakeKubeVirt completes every migration it sees with the given phase. On
// success the VMI reports landOn (or the requested node if landOn is empty) as
// its node. Writing the VMI through the fake client panics in its
// managed-fields converter on a KubeVirt uint64 field, so the node override is
// applied by an interceptor on Get instead.
func fakeKubeVirt(ctx context.Context, objs []client.Object, phase kubevirtv1.VirtualMachineInstanceMigrationPhase, landOn string) client.Client {
	var mu sync.Mutex
	landed := map[string]string{}
	c := fake.NewClientBuilder().
		WithScheme(schemeWithUpgrades()).
		WithObjects(objs...).
		WithStatusSubresource(&kubevirtv1.VirtualMachineInstanceMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if vmi, ok := obj.(*kubevirtv1.VirtualMachineInstance); ok {
					mu.Lock()
					defer mu.Unlock()
					if n, ok := landed[key.String()]; ok {
						vmi.Status.NodeName = n
					}
				}
				return nil
			},
		}).Build()

	go func() {
		for ctx.Err() == nil {
			var list kubevirtv1.VirtualMachineInstanceMigrationList
			if err := c.List(ctx, &list); err == nil {
				for i := range list.Items {
					m := &list.Items[i]
					if m.Status.Phase != "" {
						continue
					}
					if phase == kubevirtv1.MigrationSucceeded {
						target := landOn
						if target == "" {
							target = m.Spec.AddedNodeSelector[corev1.LabelHostname]
						}
						mu.Lock()
						landed[m.Namespace+"/"+m.Spec.VMIName] = target
						mu.Unlock()
					}
					m.Status.Phase = phase
					_ = c.Status().Update(ctx, m)
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return c
}

func migrator(c client.Client, timeout time.Duration) *Migrator {
	return &Migrator{Client: c, Timeout: timeout, PollInterval: 10 * time.Millisecond}
}

var move = planner.Move{Namespace: "ns", Name: "vm1", Group: "c/cp", From: "a", To: "b"}

func TestMigrateSuccess(t *testing.T) {
	_, vmi, _ := runningVM("ns", "vm1", "uid1", "a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fakeKubeVirt(ctx, []client.Object{vmi}, kubevirtv1.MigrationSucceeded, "")

	if err := migrator(c, 5*time.Second).Migrate(ctx, move); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var list kubevirtv1.VirtualMachineInstanceMigrationList
	if err := c.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected one migration object, got %d", len(list.Items))
	}
	m := list.Items[0]
	if m.Spec.VMIName != "vm1" || m.Spec.AddedNodeSelector[corev1.LabelHostname] != "b" {
		t.Fatalf("unexpected migration spec %+v", m.Spec)
	}
	if m.Labels[LabelManagedBy] != ManagedByValue {
		t.Fatalf("migration is missing managed-by label: %v", m.Labels)
	}
	if !strings.HasPrefix(m.Name, "antiaffinity-vm1-") {
		t.Fatalf("unexpected name %q", m.Name)
	}
}

func TestMigrateFailedPhase(t *testing.T) {
	_, vmi, _ := runningVM("ns", "vm1", "uid1", "a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fakeKubeVirt(ctx, []client.Object{vmi}, kubevirtv1.MigrationFailed, "")

	err := migrator(c, 5*time.Second).Migrate(ctx, move)
	if err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("expected failure, got %v", err)
	}
}

func TestMigrateTimeoutAbortsMigration(t *testing.T) {
	_, vmi, _ := runningVM("ns", "vm1", "uid1", "a")
	c := newClient(vmi)

	err := migrator(c, 100*time.Millisecond).Migrate(context.Background(), move)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
	var list kubevirtv1.VirtualMachineInstanceMigrationList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("timed-out migration should be deleted, found %d", len(list.Items))
	}
}

func TestMigrateRefusesWhenVMMoved(t *testing.T) {
	_, vmi, _ := runningVM("ns", "vm1", "uid1", "c")
	c := newClient(vmi)
	err := migrator(c, time.Second).Migrate(context.Background(), move)
	if err == nil || !strings.Contains(err.Error(), "not \"a\" as planned") {
		t.Fatalf("expected refusal, got %v", err)
	}
	var list kubevirtv1.VirtualMachineInstanceMigrationList
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("no migration should be created")
	}
}

func TestMigrateDetectsWrongLanding(t *testing.T) {
	_, vmi, _ := runningVM("ns", "vm1", "uid1", "a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fakeKubeVirt(ctx, []client.Object{vmi}, kubevirtv1.MigrationSucceeded, "elsewhere")

	err := migrator(c, 5*time.Second).Migrate(ctx, move)
	if err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("expected wrong-landing error, got %v", err)
	}
}
