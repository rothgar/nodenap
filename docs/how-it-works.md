# How it works

nodenap runs one loop (every `interval`, 15s by default) on one
leader-elected replica:

1. **Gather** Nodes, Pods, Machines and the PowerPolicy.
2. **Measure** every running machine's sensor.
3. **Price** the next watt from the tariff or live price, solar surplus and
   battery ([solar and batteries](solar-and-batteries.md)).
4. **Plan** which machines should be on or off.
5. **Step** each machine's state machine toward its target, and write its
   status.

## Waking machines

A pod that has been `Unschedulable` for `scaleUp.pendingPodGracePeriod` is
simulated against every powered-off machine's **cached capabilities**:
labels, taints, allocatable resources (including extended resources such as
`nvidia.com/gpu`) and the DaemonSet pods that will start when it boots. The
check covers:

- resource requests (with init containers, sidecars and pod overhead);
- `nodeSelector` and required node affinity;
- NoSchedule/NoExecute taints and tolerations;
- host ports and the pod limit.

The scaler then picks machines greedily by **cost per pod served**: the
machine's estimated watts × the current marginal price, divided by how many
pending pods it can take. Ties go to the faster-booting, then the
higher-`priority` machine. Pods already covered by a booting machine don't
wake another one. Pods no machine can run are listed in
`status.unsatisfiablePods` and never wake anything.

`headroom` wakes the cheapest capacity whenever spare CPU/memory across Ready
nodes drops below it.

### Pod groups

Some workloads only run when all their pods run, such as a
[LeaderWorkerSet](https://lws.sigs.k8s.io/) group spread over two machines.
nodenap finds these groups from the label their controller puts on every
member, so there's nothing to configure:
`leaderworkerset.sigs.k8s.io/group-key` (LeaderWorkerSet) or
`scheduling.x-k8s.io/pod-group` (scheduler-plugins coscheduling).

- **Waking:** machines are woken for a group only when every pending member
  fits on a booting or powered-off machine. Otherwise none are, and the group
  is listed in `status.unsatisfiablePods` (`only 1 of 2 pending pods can be
  placed`).
- **Powering off:** a node running part of a group stays on, because evicting
  one member restarts the whole group. When the group goes away (for example
  the LeaderWorkerSet scales to 0), its nodes become unneeded together.

A LeaderWorkerSet with `size: 1` isn't treated as a group.

### Upgrades

A machine that's off misses OS and Kubernetes upgrades rolled out to the
others (Omni, `talosctl upgrade`, k3s's system-upgrade-controller…). Its Node
object keeps the versions it last reported (`osImage` and `kubeletVersion`),
so when a Ready node runs a newer OS (same OS name) or kubelet, the machine
is woken to catch up (`upgrade: Talos v1.14.5 → v1.15.0`). It stays on until
its node reports the new version, for up to an hour, then is powered off as
usual once unneeded.

A machine is woken at most once for the same old versions, recorded in
`status.upgradeWake`, so one deliberately kept on an older version doesn't
wake over and over.

## Powering machines off

A machine is **unneeded** when all of these hold:

- its pods' CPU and memory requests are below
  `scaleDown.utilizationThresholdPercent`;
- every pod on it (except DaemonSet, static and ignored pods) fits on the
  other running nodes;
- no pod is bare (no controller), part of a [pod group](#pod-groups), or
  annotated `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`;
- powering it off saves at least `scaleDown.minSavingsPerHour`;
- it isn't catching up with an [upgrade](#upgrades).

After `unneededTime`, the **least efficient machine (most watts per core)**
is powered off first, within `maxConcurrent`, `minNodesOn` and `headroom`. Nothing
is powered off while machines are being woken, or within `delayAfterScaleUp`
of a wake.

## A machine's life

```
On ─(target Off)→ Draining ─(pods gone)→ ShuttingDown ─(Node NotReady)→ Off
Off ─(target On)→ Booting ─(Node Ready)→ On
Booting ─(every powerOn driver timed out)→ Failed ─(backoff)→ Off → Booting …
```

- **Draining:** the node is cordoned and tainted
  `nodenap.rothgar.github.io/off:NoSchedule`, and pods are evicted through the
  Eviction API, so PodDisruptionBudgets are respected. If they haven't left
  within `drainTimeout`, the drain is abandoned and the machine stays on.
- **ShuttingDown:** the `shutdown` driver is called. Once the Node is
  NotReady (and `powerCutDelay` has passed) the `powerCut` driver runs, if
  set. If the OS hasn't stopped by `shutdownTimeout`, power is cut (or the
  machine goes `Failed` if there's no `powerCut`).
- **Off:** the taint becomes `NoExecute`, because some pods tolerate every
  NoSchedule taint and would otherwise be scheduled onto the dead node. Pods
  still bound to the node a minute after it went NotReady are force-deleted
  so they reschedule, **but only if the scaler itself powered the machine
  off** (`status.poweredOffAt`). A node that's merely unreachable may still be
  running them.
- **Booting:** the taint goes back to `NoSchedule`, so DaemonSets that only
  tolerate NoSchedule (your CNI, monitoring agents, CSI node plugins) start and
  the Node can become Ready, while ordinary pods still wait. `powerOn` drivers
  are tried in order. The current one is
  re-sent every loop (Wake-on-LAN packets get lost); after `bootTimeout`, the
  next one is tried.
- **On:** the taint is removed. The node is uncordoned only if the scaler
  cordoned it, so a manual cordon survives a power cycle.

If a machine powers up by itself, the scaler adopts it as On. After a scaler
restart, each machine resumes from its recorded state.

## Safety rules

- Control-plane nodes, the node the scaler runs on, `alwaysOn` Machines and
  `neverPowerOffSelector` nodes are never powered off.
- Nodes without a Machine are counted as capacity but never touched.
- Dry run, or no PowerPolicy at all: observe only. No Node changes, no
  evictions, no driver calls. A machine caught mid-transition is left as it is.
- Secrets are read uncached and only from the scaler's namespace.

## Metrics

Exported on `:18080/metrics`:

| Metric | |
|---|---|
| `nodenap_machine_state{machine,state}` | 1 for the current state |
| `nodenap_machine_watts{machine,source="measured"}` | sensor readings |
| `nodenap_price_per_kwh` | import price now |
| `nodenap_effective_price_per_kwh` | marginal price of extra load now |
| `nodenap_grid_power_watts`, `nodenap_battery_soc_percent` | energy readings |
| `nodenap_machines_watts`, `nodenap_cost_per_hour` | all machines |
| `nodenap_pending_pods` | pods considered for wake-ups |
| `nodenap_actions_total{target,dry_run}` | decisions |
| `nodenap_errors_total` | failed loop runs |
| `nodenap_energy_saved_kwh_total{machine}`, `nodenap_cost_saved_total` | savings ([savings and telemetry](savings-and-telemetry.md)) |
| `nodenap_off_seconds_total{machine}`, `nodenap_wakes_total`, `nodenap_failed_boots_total`, `nodenap_wake_duration_seconds` | availability |
| `nodenap_lifetime_*`, `nodenap_savings_since_timestamp_seconds` | all-time savings totals (gauges mirroring `status.savings`) |

Cost per machine in Grafana:
`nodenap_machine_watts / 1000 * on() group_left nodenap_price_per_kwh`.

## Troubleshooting and FAQ

**It wants to power off a machine running something big (an LLM server, a
database).** The planner uses resource *requests*. A pod that requests
nothing looks tiny and "fits" anywhere. Give such workloads realistic memory
requests: for vLLM roughly `--gpu-memory-utilization` × device memory plus a
few GiB for the process.

**Pod affinity, anti-affinity and topology spread aren't simulated.** A pod
using them may fit in the simulation but not in reality. If that matters,
add requests, `nodeSelector`/affinity, or `ignorePods`, or set
`scaleDown.disabled` while you tune it.

**A pod keeps a machine on forever.** Usually a per-machine helper deployed
as a Deployment (a hardware manager, an exporter) that can only run on that
machine. Add it to `ignorePods`.

**A pod keeps waking a machine.** Same fix: `ignorePods`. Check
`status.lastActions` for the pod named in the reason.

**Helm / Flux releases with DaemonSets while machines are off.** A
DaemonSet wants a pod on every node it tolerates, and Kubernetes makes every
DaemonSet tolerate the "unreachable" and "unschedulable" taints a powered-off
node normally has. So a pod sits Pending there, and tools that wait for a
release to be ready (Helm `--wait`, Flux HelmReleases by default) time out.

nodenap's `nodenap.rothgar.github.io/off:NoExecute` taint on off machines fixes
this for most DaemonSets: one that doesn't tolerate `NoExecute` stops wanting
a pod on the off machine and reports ready. Only DaemonSets that tolerate
*every* taint (`operator: Exists` with no key or effect, often kube-proxy and
some CSI or device plugins) still count it. List them with:

```sh
kubectl get ds -A -o json | jq -r '.items[] | select(any(.spec.template.spec.tolerations[]?;
  .operator=="Exists" and (.key // "")=="" and ((.effect // "")=="" or .effect=="NoExecute")))
  | .metadata.namespace + "/" + .metadata.name'
```

If one of those is deployed by a release that waits, turn the wait off for
that release, e.g. in Flux:

```yaml
spec:
  install: {disableWait: true}
  upgrade: {disableWait: true}
```

**Wake-on-LAN doesn't work.** See the checklist in
[power drivers](power-drivers.md#wakeonlan). Test from a full power-off by
hand.

**A machine is stuck in `Failed`.** Every `powerOn` driver timed out. Check
`kubectl describe machine <name>` events, fix the driver, and the scaler
retries after the backoff (1, 2, 4… minutes, max 1h).

**Prices look wrong.** Compare `status.price` against your tariff at a few
times of day, check `energy.timeZone`, and read
[finding your power costs](power-costs.md).
