#!/usr/bin/env bash
# End-to-end test: real KubeVirt live migrations in kind, driven by the
# deploy/ manifests, with the kind API server standing in for Rancher.
#
#   hack/e2e.sh            create/reuse the cluster and run the test
#   KEEP_CLUSTER=1 ...     leave the cluster running afterwards
set -euo pipefail

CLUSTER=${CLUSTER:-hvaa-e2e}
KUBEVIRT_VERSION=${KUBEVIRT_VERSION:-v1.8.4}
CIRROS_IMAGE=${CIRROS_IMAGE:-quay.io/kubevirt/cirros-container-disk-demo:${KUBEVIRT_VERSION}}
IMAGE=harvester-antiaffinity:e2e
VM_NS=e2e-vms
APP_NS=harvester-antiaffinity
ROOT=$(cd "$(dirname "$0")/.." && pwd)
ARTIFACTS=${ARTIFACTS:-$ROOT/.e2e}
export KUBECONFIG=$ARTIFACTS/kubeconfig

mkdir -p "$ARTIFACTS"
log() { printf '\n=== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

dump() {
  log "collecting diagnostics into $ARTIFACTS"
  kubectl get nodes -o wide >"$ARTIFACTS/nodes.txt" 2>&1 || true
  kubectl get vm,vmi,vmim -A -o wide >"$ARTIFACTS/kubevirt.txt" 2>&1 || true
  kubectl get vmim -A -o yaml >"$ARTIFACTS/vmim.yaml" 2>&1 || true
  kubectl -n "$APP_NS" get jobs,pods -o wide >"$ARTIFACTS/jobs.txt" 2>&1 || true
  kubectl -n "$APP_NS" logs -l app.kubernetes.io/name=harvester-antiaffinity --tail=-1 --prefix >"$ARTIFACTS/tool.log" 2>&1 || true
  kubectl -n kubevirt get pods -o wide >"$ARTIFACTS/kubevirt-pods.txt" 2>&1 || true
}

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then dump; fi
  if [[ -z "${KEEP_CLUSTER:-}" ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  exit $rc
}
trap cleanup EXIT

log "cluster"
if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --config "$ROOT/test/e2e/kind.yaml" --kubeconfig "$KUBECONFIG" --wait 5m
else
  kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
fi

log "build and load image"
docker build -t "$IMAGE" "$ROOT"
kind load docker-image "$IMAGE" --name "$CLUSTER"

log "install KubeVirt $KUBEVIRT_VERSION"
kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-operator.yaml"
kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-cr.yaml"
# kind nodes rarely expose /dev/kvm; software emulation is slow but enough.
kubectl -n kubevirt patch kubevirt kubevirt --type merge \
  -p '{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}'
kubectl -n kubevirt wait kv kubevirt --for condition=Available --timeout=15m

log "fake rancher"
kubectl apply -f "$ROOT/test/e2e/fake-rancher.yaml"

mapfile -t WORKERS < <(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort)
[[ ${#WORKERS[@]} -eq 3 ]] || fail "expected 3 worker nodes, got ${#WORKERS[@]}"
HOT=${WORKERS[0]}

log "create VMs, all on $HOT"
kubectl create namespace "$VM_NS" --dry-run=client -o yaml | kubectl apply -f -
for n in "${WORKERS[@]:1}"; do kubectl cordon "$n"; done

vm() {
  local name=$1 annotations=${2:-"{}"}
  cat <<EOF
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: $name
  namespace: $VM_NS
  annotations: $annotations
spec:
  runStrategy: Always
  template:
    spec:
      domain:
        devices:
          disks:
            - name: containerdisk
              disk: {bus: virtio}
            - name: cloudinit
              disk: {bus: virtio}
          interfaces:
            - name: default
              masquerade: {}
        resources:
          requests:
            memory: 128Mi
      networks:
        - name: default
          pod: {}
      volumes:
        - name: containerdisk
          containerDisk:
            image: $CIRROS_IMAGE
        - name: cloudinit
          cloudInitNoCloud:
            userData: |
              #!/bin/sh
              echo e2e
---
EOF
}

CP_VMS=(e2e-cp-1 e2e-cp-2 e2e-cp-3)
WORKER_VMS=(e2e-w-1 e2e-w-2 e2e-w-3)
PINNED=e2e-w-pinned
BYSTANDER=bystander
{
  for v in "${CP_VMS[@]}" "${WORKER_VMS[@]}" "$BYSTANDER"; do vm "$v"; done
  vm "$PINNED" '{"harvester-antiaffinity.jaevans.github.io/ignore": "true"}'
} | kubectl apply -f -

ALL_VMS=("${CP_VMS[@]}" "${WORKER_VMS[@]}" "$PINNED" "$BYSTANDER")
for v in "${ALL_VMS[@]}"; do
  until kubectl -n "$VM_NS" get vmi "$v" >/dev/null 2>&1; do sleep 2; done
done
kubectl -n "$VM_NS" wait vmi --all --for condition=Ready --timeout=15m
kubectl -n "$VM_NS" wait vmi --all --for condition=LiveMigratable --timeout=5m
for n in "${WORKERS[@]:1}"; do kubectl uncordon "$n"; done

node_of() { kubectl -n "$VM_NS" get vmi "$1" -o jsonpath='{.status.nodeName}'; }
for v in "${ALL_VMS[@]}"; do
  [[ $(node_of "$v") == "$HOT" ]] || fail "$v did not start on $HOT"
done

log "register machines with fake rancher"
machine() {
  local name=$1 pool=$2 etcd=$3 uid
  uid=$(kubectl -n "$VM_NS" get vm "$name" -o jsonpath='{.metadata.uid}')
  cat <<EOF
apiVersion: rke-machine.cattle.io/v1
kind: HarvesterMachine
metadata:
  name: $name
  namespace: fleet-default
  labels:
    rke.cattle.io/cluster-name: e2e
    rke.cattle.io/rke-machine-pool-name: $pool
    rke.cattle.io/etcd-role: "$etcd"
spec:
  vmNamespace: $VM_NS
  providerID: harvester://$uid
---
EOF
}
{
  for v in "${CP_VMS[@]}"; do machine "$v" control-plane true; done
  for v in "${WORKER_VMS[@]}" "$PINNED"; do machine "$v" workers false; done
} | kubectl apply -f -

log "deploy tool"
kubectl apply -k "$ROOT/test/e2e/overlay"
until kubectl -n fleet-default get secret rancher-reader-token -o jsonpath='{.data.token}' | grep -q .; do sleep 1; done
TOKEN=$(kubectl -n fleet-default get secret rancher-reader-token -o jsonpath='{.data.token}' | base64 -d)
kubectl -n fleet-default get secret rancher-reader-token -o jsonpath='{.data.ca\.crt}' | base64 -d >"$ARTIFACTS/ca.crt"
kubectl -n "$APP_NS" create secret generic rancher-credentials \
  --from-literal=url=https://kubernetes.default.svc \
  --from-literal=token="$TOKEN" \
  --from-file=ca.crt="$ARTIFACTS/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f -

run_job() {
  local name=$1
  kubectl -n "$APP_NS" delete job "$name" --ignore-not-found
  kubectl -n "$APP_NS" create job "$name" --from=cronjob/harvester-antiaffinity
  if ! kubectl -n "$APP_NS" wait job "$name" --for condition=Complete --timeout=20m; then
    kubectl -n "$APP_NS" logs "job/$name" --tail=-1 >"$ARTIFACTS/$name.log" 2>&1 || true
    fail "job $name did not complete (see $ARTIFACTS/$name.log)"
  fi
  kubectl -n "$APP_NS" logs "job/$name" --tail=-1 | tee "$ARTIFACTS/$name.log"
}

log "first run: expect migrations"
run_job e2e-run-1

declare -A seen
for v in "${CP_VMS[@]}"; do seen[$(node_of "$v")]=1; done
[[ ${#seen[@]} -eq 3 ]] || fail "control-plane VMs not on 3 distinct nodes: $(for v in "${CP_VMS[@]}"; do printf '%s=%s ' "$v" "$(node_of "$v")"; done)"

[[ $(node_of "$PINNED") == "$HOT" ]] || fail "opted-out VM $PINNED was moved"
[[ $(node_of "$BYSTANDER") == "$HOT" ]] || fail "VM $BYSTANDER outside any pool was moved"

declare -A wcount=()
for n in "${WORKERS[@]}"; do wcount[$n]=0; done
for v in "${WORKER_VMS[@]}" "$PINNED"; do n=$(node_of "$v"); wcount[$n]=$((wcount[$n] + 1)); done
min=99 max=0
for n in "${WORKERS[@]}"; do (( wcount[$n] < min )) && min=${wcount[$n]}; (( wcount[$n] > max )) && max=${wcount[$n]}; done
(( max - min <= 1 )) || fail "worker pool unbalanced: $(declare -p wcount)"

grep -q '"msg":"live migration succeeded"' "$ARTIFACTS/e2e-run-1.log" || fail "no successful migration logged"
if grep -q '"msg":"live migration failed"' "$ARTIFACTS/e2e-run-1.log"; then fail "a migration failed"; fi

log "second run: expect no migrations"
run_job e2e-run-2
grep -q '"msg":"all groups are balanced"' "$ARTIFACTS/e2e-run-2.log" || fail "second run was not a no-op"
if grep -q '"msg":"starting live migration"' "$ARTIFACTS/e2e-run-2.log"; then fail "second run migrated"; fi

dump
log "PASS"
