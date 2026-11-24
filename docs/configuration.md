# Configuration reference

Two cluster-scoped resources in `power.nodenap.rothgar.github.io/v1alpha1`:

- **PowerPolicy**: one per cluster (if there are several, the first by name
  is used). Without one, the scaler only observes.
- **Machine**: one per physical machine it may switch.

Prices are decimal strings (`"0.25"`) per kWh, in your currency. Durations
use Go syntax (`30s`, `15m`, `4m15s`).

## PowerPolicy

| Field | Default | Meaning |
|---|---|---|
| `dryRun` | `false` | Decide and report, but never act. **Start with `true`.** |
| `interval` | `15s` | How often the loop runs. |
| `nodeSelector` | all nodes | Nodes counted as capacity. |
| `neverPowerOffSelector` | none | Nodes never powered off. Control-plane nodes are always protected. |
| `ignorePods[]` | none | `{namespace, selector}` pods that never wake a machine and never keep one on. The pod annotation `nodenap.rothgar.github.io/ignore: "true"` does the same. |
| `minNodesOn` | `0` | Minimum Ready nodes kept on. |
| `headroom.cpu` / `.memory` | none | Spare schedulable capacity (summed over Ready nodes) to keep on. |
| `scaleUp.pendingPodGracePeriod` | `30s` | How long a pod must be unschedulable before machines are woken for it. |
| `scaleUp.bootTimeout` | `10m` | Per `powerOn` driver; see also `Machine.bootTimeout`. |
| `scaleUp.maxConcurrent` | `3` | Machines booting at once. |
| `scaleDown.disabled` | `false` | Never power machines off. |
| `scaleDown.utilizationThresholdPercent` | `50` | A machine is a candidate when its pods' CPU and memory **requests** are both below this % of allocatable. |
| `scaleDown.unneededTime` | `15m` | How long a machine must stay unneeded first. |
| `scaleDown.delayAfterScaleUp` | `10m` | No power-offs this soon after a wake. |
| `scaleDown.maxConcurrent` | `1` | Machines powering off at once. |
| `scaleDown.drainTimeout` | `10m` | Give up and keep the machine on if pods haven't left. |
| `scaleDown.shutdownTimeout` | `5m` | Time for the OS to shut down before `powerCut` (or `Failed`). |
| `scaleDown.minSavingsPerHour` | `"0"` | Skip power-offs saving less than this per hour. |
| `energy.timeZone` | `UTC` | IANA zone (e.g. `America/Los_Angeles`) that tariff window times, days and months are in; daylight saving is handled. Status timestamps stay UTC, as everywhere in Kubernetes. |
| `energy.currency` | | Display only. |
| `energy.tariff.defaultPrice` | | Price outside all windows. |
| `energy.tariff.windows[]` | | `{start, end, price, days?, months?}`; first match wins; `end` before `start` wraps midnight; `start == end` is the whole day. |
| `energy.price` | | A [Value](#values): live price; wins over the tariff when readable. |
| `energy.exportPrice` | `"0"` | Credit per exported kWh. |
| `energy.netMetering.nonBypassableChargesPerKWh` | | Retail net metering: exports earn the import price minus this. |
| `energy.gridPower` | | Value: watts at the meter, **+ exporting / − importing**. |
| `energy.battery.soc` | | Value: state of charge, %. |
| `energy.battery.power` | | Value: watts, + discharging. |
| `energy.battery.reserveSoCPercent` | `0` | Below this the battery isn't counted on. |
| `energy.battery.costPerKWh` | grid price | What a kWh from the battery is worth. |
| `integrations.prometheusURL` | | For `prometheus` Values and sensors. |
| `integrations.homeAssistantURL` | | For `homeAssistant` Values. |
| `integrations.homeAssistantTokenSecretRef` | | Long-lived token (`key` default `token`). |
| `telemetry.enabled` | `false` | Opt-in anonymous daily report ([savings and telemetry](savings-and-telemetry.md)). |
| `telemetry.endpoint` | | Collector URL. |

### Status

| Field | |
|---|---|
| `price`, `effectivePrice` | Import price now, and the marginal price of 100 extra watts (solar/battery aware). |
| `gridPowerWatts`, `batterySoCPercent` | Latest readings. |
| `machinesWatts`, `costPerHour` | All machines, measured or estimated. |
| `nodesOn`, `nodesOff`, `pendingPods` | |
| `lastActions[]` | Decisions from the last run, with reasons (in dry run: what would happen). |
| `suggestions[]` | Report-only: what would happen to worker nodes that have no Machine yet (which could sleep, or should wake), with size-based watt estimates. |
| `unsatisfiablePods[]` | Pending pods no machine can run, and why. |
| `warnings[]` | Problems reading prices or energy data. |
| `error` | Invalid configuration. |
| `savings` | Running totals: `energyKWh`, `cost`, `machineHoursOff`, `wakes`, `failedBoots`, since `since`; plus the telemetry `installID` and last report. |

## Machine

`metadata.name` is the Kubernetes node name unless `spec.nodeName` is set.

| Field | Default | Meaning |
|---|---|---|
| `nodeName` | `metadata.name` | |
| `powerOn[]` | | [Drivers](power-drivers.md) tried in order. |
| `shutdown` | | Driver for a clean OS shutdown. |
| `powerCut` | | Driver that cuts power after shutdown (or on shutdown timeout). |
| `powerCutDelay` | `30s` | Wait after the node goes NotReady before `powerCut`. |
| `sensor` | | Driver for watts ([power sensors](power-sensors.md)). |
| `bootTimeout` | policy's | Per-machine override. |
| `alwaysOn` | `false` | Never power off (still measured). |
| `priority` | `0` | Tie-breaker: higher stays on / is woken first. |
| `profile.idleWatts` | 5 W + 2 W per CPU core | Watts when on and idle. Measure it if you can; the default is a rough size-based guess. |
| `profile.wattsPerCore` | `4` | Extra watts per busy (requested) CPU core. |
| `profile.maxWatts` | | Caps the estimate. |
| `profile.offWatts` | `0` | Standby watts while off. |
| `profile.bootTime` | `3m` | Used to prefer faster-booting machines. |

Annotation `nodenap.rothgar.github.io/override: "on"` or `"off"` forces the target
(remove the annotation to go back to automatic).

### Status

| Field | |
|---|---|
| `powerState` | `On`, `Draining`, `ShuttingDown`, `Off`, `Booting`, `Failed` (or `Unknown` before first adoption). |
| `target`, `targetReason` | What the planner (or override) wants, and why. |
| `message`, `lastTransitionTime` | |
| `unneededSince` | When the planner first saw it as unneeded. |
| `poweredOffAt` | Set when the scaler powered it off; only then are stranded pods force-deleted. |
| `powerOnAttempt`, `failureCount` | Boot progress and failures (exponential backoff, max 1h). |
| `upgradeWake` | Versions the node reported when it was last woken to catch up with an [upgrade](how-it-works.md#upgrades), and when. |
| `cachedNode` | Labels, taints, allocatable and DaemonSet overhead as last seen while on: what the planner schedules against while it's off. |
| `measuredWatts`, `measuredAt` | Latest sensor reading. |

## Values

A Value reads one number. Set one source, plus an optional `scale`:

```yaml
prometheus: {query: 'sum(solar_watts)'}
homeAssistant: {entity: sensor.x}                 # or entities: [a, b] (summed); attribute: name
http: {url: http://..., field: a.b.0.c}           # same fields as the http driver
static: "0.12"
scale: "-1000"                                    # optional multiplier
```
