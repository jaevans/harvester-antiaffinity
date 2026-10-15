# harvester-antiaffinity

Rancher can put several VMs from one downstream-cluster machine pool on the
same Harvester node. Lose that node and you lose all of them at once: for
example, two of the three etcd members of a cluster.

`harvester-antiaffinity` runs inside Harvester as a CronJob. It finds out from
Rancher which VMs belong to the same machine pool, then live-migrates VMs so
that each pool is spread as evenly as possible across Harvester nodes. "Evenly"
means the per-node count for a pool differs by at most one.

## How it works

Each run:

1. **Discover pools.** It lists `HarvesterMachine` objects
   (`rke-machine.cattle.io/v1`) in `fleet-default` on the Rancher management
   cluster, through Rancher's `/k8s/clusters/local` proxy. Each object's name is
   the Harvester VM name. Its labels give the downstream cluster
   (`rke.cattle.io/cluster-name`), the pool (`rke.cattle.io/rke-machine-pool-name`)
   and the roles. `spec.vmNamespace` gives the VM namespace. `spec.providerID`
   (`harvester://<uid>`) must match the Harvester VirtualMachine UID. A machine
   without a providerID yet is skipped, so a same-named VM on a different
   Harvester cluster is never touched.
2. **Read Harvester state.** It reads nodes, pods, VMs, VMIs and migrations.
   A node is a migration target only if it is Ready, not cordoned, not in
   Harvester maintenance mode, and passes the VM's required node affinity,
   node selector and taint tolerations. Free memory is node allocatable minus
   pod requests. Scheduling constraints come from the running virt-launcher
   pod, which carries the selectors KubeVirt adds (such as
   `kubevirt.io/schedulable` and host CPU model labels). A VM is movable only if its VMI is Running, Ready and
   `LiveMigratable`, and no migration is already in progress.
3. **Plan.** Pools are balanced in priority order: etcd pools, then control
   plane, then everything else. The planner moves a VM from a fullest node to an
   emptiest allowed node with enough free memory while the gap is two or more.
4. **Migrate, one at a time.** Each move creates a
   `VirtualMachineInstanceMigration` with
   `spec.addedNodeSelector: {kubernetes.io/hostname: <target>}`, which is what
   Harvester's own "Migrate to node" action does. The tool waits for it to
   finish and checks that the VM landed on the target. If the timeout passes,
   it deletes the migration, which aborts it. After a success it waits
   `--settle-time` (default 2m) so that etcd and other guest services recover.
   Then it re-reads state and re-plans before the next move.

VMs on a node that is not eligible (not Ready, cordoned, in maintenance) are
neither moved nor counted. Balance is computed over the nodes that can
actually take VMs.

### Safety rails

- It does nothing while a Harvester upgrade is in progress.
- It does nothing while **any** migration in the cluster is unfinished. That
  includes migrations started by Harvester maintenance mode, KubeVirt
  workload updates or users, and aborted migrations still being torn down.
- It does nothing while a node is entering maintenance mode.
- At most one migration runs at a time.
- `--max-migrations` (default 10) caps migrations per run, and
  `--max-failures` (default 3) stops a run after that many failures.
- A VM whose migration fails is not retried in the same run.
- To opt a VM out, annotate its VirtualMachine with
  `harvester-antiaffinity.jaevans.github.io/ignore: "true"`. It is still
  counted, so the rest of its pool balances around it.
- `--dry-run` logs the plan without migrating.

## Deploy

The manifests in `deploy/` create a namespace, RBAC and a CronJob that runs at
17 minutes past every third hour. The CronJob ships with `DRY_RUN=true`. Check
the first run's logs, then set it to `false`.

1. Create a Rancher API token that can read `harvestermachines` in
   `fleet-default` on the `local` cluster. Then create the secret:

   ```sh
   kubectl create namespace harvester-antiaffinity
   kubectl -n harvester-antiaffinity create secret generic rancher-credentials \
     --from-literal=url=https://rancher.example.com \
     --from-literal=token=token-xxxxx:yyyy
   # optional, for a private CA: --from-file=ca.crt=ca.pem
   # and set RANCHER_CA_FILE=/var/run/secrets/rancher/ca.crt on the CronJob
   ```

2. Apply the manifests to the Harvester cluster:

   ```sh
   kubectl apply -k deploy/
   ```

3. Trigger a run and read the plan:

   ```sh
   kubectl -n harvester-antiaffinity create job --from=cronjob/harvester-antiaffinity manual-1
   kubectl -n harvester-antiaffinity logs -f job/manual-1
   ```

The image is published to `ghcr.io/jaevans/harvester-antiaffinity`. Git tag
`vX.Y.Z` produces image tag `X.Y.Z`, and `main` produces `latest` and
`sha-<commit>`. The CronJob pins a version tag. If the
package is private, add an `imagePullSecret` or make the package public.

To run it as a long-lived Deployment instead, set `INTERVAL` (for example
`3h`). The process then runs one pass per interval instead of exiting.

## Configuration

Every flag can also be set by an environment variable: upper-case the flag
name and replace `-` with `_`. A command-line flag wins over the environment.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--source` | `rancher` | `rancher`, or `labels` to group by the `harvesterhci.io/machineSetName` VM label without contacting Rancher (roles are then unknown, so all pools have equal priority) |
| `--rancher-url` | | Rancher server URL |
| `--rancher-token` / `--rancher-token-file` | | Rancher bearer token |
| `--rancher-ca-file` | | PEM CA bundle for Rancher |
| `--rancher-insecure-skip-verify` | `false` | Skip TLS verification |
| `--rancher-cluster-path` | `/k8s/clusters/local` | Path under the URL to the management cluster API |
| `--rancher-namespace` | `fleet-default` | Where HarvesterMachines live; empty for all namespaces |
| `--include-clusters` / `--exclude-clusters` | | Comma-separated downstream cluster names |
| `--dry-run` | `false` | Plan only |
| `--max-migrations` | `10` | Per run, 0 = unlimited |
| `--max-failures` | `3` | Stop a run after this many failures, 0 = unlimited |
| `--migration-timeout` | `30m` | Abort a single migration after this long (the CronJob sets 15m) |
| `--poll-interval` | `5s` | Migration status poll interval |
| `--settle-time` | `2m` | Pause after each successful migration |
| `--interval` | `0` | Run continuously with this period; 0 = run once |
| `--kubeconfig` | in-cluster | Harvester kubeconfig, for running outside the cluster |
| `--log-level` / `--log-format` | `info` / `json` | |

To try it from a workstation against a real cluster:

```sh
go run ./cmd/harvester-antiaffinity --dry-run --log-format text \
  --kubeconfig ~/.kube/harvester.yaml \
  --rancher-url https://rancher.example.com --rancher-token-file ./token
```

## Also fix the pool's own anti-affinity

Rancher's Harvester machine config has a `vmAffinity` field. A preferred
pod-anti-affinity term **without a `labelSelector`** matches no pods, so it has
no effect. Give the term a selector such as
`harvesterhci.io/machineSetName: <vm-namespace>-<cluster>-<pool>` so new VMs
are placed well from the start. This tool then only has to correct drift, for
example after node maintenance.

## Development

```sh
make test   # unit tests (race detector)
make lint   # golangci-lint
make e2e    # kind + KubeVirt end-to-end test, needs Docker and kind
```

The e2e test needs amd64 or real KVM. On arm64, KubeVirt only accepts
`host-passthrough` CPUs, which software emulation cannot provide. Apple
Silicon before M3 has no nested virtualization, so the test cannot run on
those Macs; the script stops early and says so. CI runs it on amd64.

virt-handler crashes in kind with "too many open files" unless the Docker
host's inotify limits are raised:
`sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288`.
On macOS, run that in the Docker VM, for example
`docker run --rm --privileged --pid=host alpine sysctl -w ...`.

The e2e test (`hack/e2e.sh`) works like this:

1. It creates a four-node kind cluster and installs KubeVirt in emulation mode.
2. The kind API server stands in for Rancher: the test installs the
   HarvesterMachine CRD and a token that can only read it.
3. It starts eight VMs on one worker: three in an etcd pool, four in a worker
   pool (one of them opted out), and one bystander that belongs to no pool.
4. It runs the real CronJob manifests and asserts the following:
   - The etcd pool ends up on three distinct nodes.
   - The worker pool is balanced.
   - The opted-out VM and the bystander have not moved.
   - A second run is a no-op.
