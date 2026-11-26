#!/usr/bin/env bash
# End-to-end tests on k3s (k3d). The "machine" is a k3d agent container; a
# fake smart plug on the host (hack/e2e/plug.py) stops and starts it when
# nodenap's http driver calls it, the way a Shelly or Tasmota plug would cut
# and restore power.
#
#   hack/e2e-k3s.sh power     idle machine powered off, woken by a pod that
#                             needs it, powered off again when the pod's gone
#   hack/e2e-k3s.sh upgrade   a powered-off machine on an older k3s (OS image
#                             and kubelet) is woken to catch up, and kept on
#
# Needs docker, k3d, kubectl, helm, curl and python3. Run from the repo root.
# KEEP=1 leaves the cluster running afterwards.
set -euo pipefail

SCENARIO=${1:?usage: $0 power|upgrade}
CLUSTER=${CLUSTER:-nodenap-e2e-$SCENARIO}
K3S_NEW=${K3S_NEW:-rancher/k3s:v1.33.5-k3s1}
K3S_OLD=${K3S_OLD:-rancher/k3s:v1.32.9-k3s1}
PLUG_PORT=${PLUG_PORT:-8099}
IMG=nodenap:e2e
WORK=$(mktemp -d)
PLUG_LOG=$WORK/plug.log
: >"$PLUG_LOG"

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
field() { kubectl get "machine/$NODE" -o jsonpath="$1"; }
off_taint() { kubectl get node "$NODE" -o jsonpath='{.spec.taints}' | grep -o 'nodenap.rothgar.github.io/off[^}]*' || true; }
container_running() { docker inspect -f '{{.State.Running}}' "$NODE"; }

diagnose() {
  set +e
  step "diagnostics"
  kubectl get nodes -o wide
  kubectl get machines -o wide
  kubectl get machine "$NODE" -o yaml
  kubectl get powerpolicy default -o yaml
  kubectl get pods -A -o wide
  kubectl get events -A --sort-by=.lastTimestamp | tail -40
  kubectl -n nodenap logs deploy/nodenap --tail=200
  echo "--- plug log"; cat "$PLUG_LOG"
  echo "--- plug server"; cat "$WORK/plug.out"
}

cleanup() {
  status=$?
  [ $status -ne 0 ] && diagnose
  [ -n "${PLUG_PID:-}" ] && kill "$PLUG_PID" 2>/dev/null
  [ -z "${KEEP:-}" ] && k3d cluster delete "$CLUSTER" >/dev/null 2>&1
  rm -rf "$WORK"
  exit $status
}
trap cleanup EXIT

# Notice a stopped node in 20s instead of 40s+. No traefik/servicelb: fewer
# pods to move around.
create_cluster() {
  k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
  k3d cluster create "$CLUSTER" --image "$K3S_NEW" --agents "$1" --no-lb --wait \
    --k3s-arg '--disable=traefik,servicelb@server:0' \
    --k3s-arg '--kube-controller-manager-arg=node-monitor-grace-period=20s@server:0'
}

setup() {
  kubectl wait --for=condition=Ready nodes --all --timeout=3m

  step "image"
  docker build -q --build-arg VERSION=e2e -t "$IMG" .
  k3d image import "$IMG" -c "$CLUSTER"

  step "fake smart plug for $NODE"
  python3 hack/e2e/plug.py "$PLUG_PORT" "$NODE" "$PLUG_LOG" >"$WORK/plug.out" 2>&1 &
  PLUG_PID=$!
  sleep 1
  curl -fsS "http://127.0.0.1:$PLUG_PORT/status?node=$NODE"; echo

  step "install nodenap"
  kubectl create namespace nodenap
  # The controller stays on the server, which is never powered off.
  helm install nodenap charts/nodenap -n nodenap --wait --timeout 3m \
    --set image.repository=nodenap --set image.tag=e2e --set image.pullPolicy=Never \
    --set hostNetwork=false --set policy.create=false --set logLevel=debug \
    --set-json 'nodeSelector={"node-role.kubernetes.io/control-plane":"true"}'

  # k3d resolves host.k3d.internal to the host from inside the cluster.
  local plug=http://host.k3d.internal:$PLUG_PORT
  kubectl apply -f - <<YAML
apiVersion: power.nodenap.rothgar.github.io/v1alpha1
kind: PowerPolicy
metadata:
  name: default
spec:
  dryRun: false
  interval: 5s
  minNodesOn: 1
  energy:
    currency: USD
    tariff: {defaultPrice: "0.30"}
  scaleUp:
    pendingPodGracePeriod: 5s
    bootTimeout: 3m
  scaleDown:
    unneededTime: 20s
    delayAfterScaleUp: 20s
    drainTimeout: 2m
    shutdownTimeout: 2m
---
apiVersion: power.nodenap.rothgar.github.io/v1alpha1
kind: Machine
metadata:
  name: $NODE
spec:
  powerOn:
    - http: {method: GET, url: "$plug/on?node=$NODE"}
  shutdown:
    http: {method: GET, url: "$plug/off?node=$NODE"}
  sensor:
    http: {url: "$plug/status?node=$NODE", field: apower}
  profile: {idleWatts: "20", maxWatts: "60", offWatts: "0.5"}
YAML
}

expect_powered_off() {
  kubectl wait "machine/$NODE" --for=jsonpath='{.status.powerState}'=Off --timeout=6m
  [ "$(grep -c "off $NODE" "$PLUG_LOG")" -ge "$1" ] || fail "the plug wasn't switched off (log: $(cat "$PLUG_LOG"))"
  [ "$(container_running)" = false ] || fail "$NODE is still running"
  off_taint | grep -q NoExecute || fail "the off node lacks the NoExecute taint: $(off_taint)"
  kubectl get machines
}

expect_powered_on() {
  kubectl wait "machine/$NODE" --for=jsonpath='{.status.powerState}'=On --timeout=6m
  grep -q "on $NODE" "$PLUG_LOG" || fail "the plug was never switched on"
  [ "$(container_running)" = true ] || fail "$NODE isn't running"
  [ -z "$(off_taint)" ] || fail "taint still on the woken node: $(off_taint)"
  kubectl get machines
}

scenario_power() {
  NODE=k3d-$CLUSTER-agent-1          # managed
  local other=k3d-$CLUSTER-agent-0   # unmanaged; takes the pods
  step "cluster ($K3S_NEW, 2 agents)"
  create_cluster 2
  setup

  step "idle machine is powered off"
  expect_powered_off 1
  [ "$(docker inspect -f '{{.State.Running}}' "$other")" = true ] || fail "unmanaged node $other was stopped"

  step "a pod that needs it wakes it"
  kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: needs-machine, namespace: default}
spec:
  replicas: 1
  selector: {matchLabels: {app: needs-machine}}
  template:
    metadata: {labels: {app: needs-machine}}
    spec:
      nodeSelector: {kubernetes.io/hostname: $NODE}
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10
          resources: {requests: {cpu: 50m, memory: 16Mi}}
YAML
  expect_powered_on
  kubectl rollout status deploy/needs-machine --timeout=3m

  step "it sleeps again once the pod is gone"
  kubectl delete deploy/needs-machine --wait
  expect_powered_off 2
}

scenario_upgrade() {
  # The managed machine runs an older k3s than the rest of the cluster, as
  # if it had been off while the others were upgraded. Its Node reports the
  # older OS image (K3s vX) and kubelet.
  NODE=k3d-oldk3s-0
  step "cluster ($K3S_NEW, 1 agent) plus $NODE on $K3S_OLD"
  create_cluster 1
  k3d node create oldk3s --cluster "$CLUSTER" --role agent --image "$K3S_OLD" --wait
  kubectl get nodes -o custom-columns='NAME:.metadata.name,OS:.status.nodeInfo.osImage,KUBELET:.status.nodeInfo.kubeletVersion'
  setup

  step "idle machine is powered off"
  expect_powered_off 1

  step "while off, it's behind the other nodes, so it's woken to catch up"
  kubectl wait "machine/$NODE" --for=jsonpath='{.status.target}'=On --timeout=2m
  reason=$(field '{.status.targetReason}')
  echo "reason: $reason"
  [[ $reason == upgrade:*kubelet\ v*→* ]] || fail "unexpected wake reason: $reason"
  old_os=$(kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.osImage}')
  new_os=$(kubectl get node "k3d-$CLUSTER-server-0" -o jsonpath='{.status.nodeInfo.osImage}')
  if [ "$old_os" != "$new_os" ]; then
    [[ $reason == *"${old_os%% *} v"*→* ]] || fail "OS upgrade missing from the reason ($old_os → $new_os): $reason"
  fi
  [ -n "$(field '{.status.upgradeWake.from}')" ] || fail "status.upgradeWake not recorded"
  expect_powered_on

  step "it's kept on to catch up (not powered off as idle)"
  sleep 75   # well past unneededTime + delayAfterScaleUp
  [ "$(field '{.status.powerState}')/$(field '{.status.target}')" = On/On ] \
    || fail "machine left On while catching up: $(field '{.status.powerState}')/$(field '{.status.target}')"
  [ "$(grep -c "off $NODE" "$PLUG_LOG")" -eq 1 ] || fail "powered off again while catching up"
  kubectl get machines
}

"scenario_$SCENARIO"
step "passed: $SCENARIO"
cat "$PLUG_LOG"
