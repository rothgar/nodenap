# Savings and telemetry

## What nodenap counts

Every loop, for each machine that **nodenap itself powered off** and
that is fully `Off`, it credits:

- **energy saved** = time off × (watts it would have drawn idling − its
  standby `profile.offWatts`). "Idling" is its last sensor reading before it
  went off, or `profile.idleWatts` without a sensor.
- **money saved** = that energy × the price at the time, after solar surplus
  and the battery ([solar and batteries](solar-and-batteries.md)).

Not credited:

- machines that were already off when the scaler found them, or that you
  powered off yourself;
- anything in dry run;
- draining, shutting down and booting (the machine is on then).

Idle watts are a deliberately conservative baseline. The workloads ran
somewhere either way; what was saved is the extra machine sitting idle.

### Where to see it

```sh
kubectl get powerpolicy default -o jsonpath='{.status.savings}' | jq
```

```json
{"since": "2026-11-23T18:04:00Z", "energyKWh": "41.207319", "cost": "15.322904",
 "machineHoursOff": "763.412500", "wakes": 37, "failedBoots": 0}
```

Totals live in the PowerPolicy status, so they survive restarts. Deleting
the PowerPolicy resets them. Metrics:

| Metric | |
|---|---|
| `nodenap_energy_saved_kwh_total{machine}` | kWh saved |
| `nodenap_cost_saved_total` | money saved, at the price at the time |
| `nodenap_off_seconds_total{machine}` | time fully off |
| `nodenap_wakes_total` | machines woken and Ready |
| `nodenap_failed_boots_total` | wakes where every powerOn driver timed out |
| `nodenap_wake_duration_seconds` | histogram, power-on to Node Ready |
| `nodenap_lifetime_energy_saved_kwh`, `nodenap_lifetime_cost_saved`, `nodenap_lifetime_machine_hours_off`, `nodenap_lifetime_wakes`, `nodenap_lifetime_failed_boots` | gauges: the lifetime totals from the status above |
| `nodenap_savings_since_timestamp_seconds` | when those totals started (`since`) |

Prometheus counters restart at zero when the controller restarts;
`increase()` and `rate()` handle that, but only as far back as Prometheus
keeps data. For all-time figures use the `nodenap_lifetime_*` gauges, which
mirror the PowerPolicy status.

### Grafana dashboard

The chart ships a dashboard: all-time savings (money, kWh, machine-hours
off, average saved per day, wakes, and when counting started, whatever the
time range), savings for the selected time range (money, kWh, machine-hours
off, wakes, median wake time), current draw, cost per hour
and price, a power-state timeline per machine, power by machine, savings by
machine, price over time, and grid/battery when configured. In dry run, the
"Would act" panel counts the decisions nodenap would have made.

With kube-prometheus-stack (or any Grafana running the dashboard sidecar):

```yaml
# Helm values
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack   # whatever your Prometheus selects on
grafanaDashboard:
  enabled: true                      # ConfigMap labelled grafana_dashboard: "1"
```

Without the sidecar, import
[`charts/nodenap/dashboards/nodenap.json`](../charts/nodenap/dashboards/nodenap.json)
in Grafana (Dashboards → New → Import). Money panels use USD formatting;
change the unit to your currency in the panel settings.

## Anonymous telemetry (opt-in)

If you'd like to add your install to the public "energy saved by nodenap"
total at <https://nodenap-collector.123dev.workers.dev>, turn on telemetry.
It is **off by default**.

```yaml
spec:
  telemetry:
    enabled: true
    endpoint: https://nodenap-collector.123dev.workers.dev/report
```

Once a day the scaler POSTs one JSON report and **logs the exact payload
first**, so you can see everything that's sent:

```json
{"installId": "6a1f0c7e-3b9d-4f44-9a0e-2d1c5b7e8f90", "version": "v0.1.0",
 "day": "2026-11-23", "report": 12, "machines": "4-7",
 "energySavedKWh": 41.207319, "machineHoursOff": 763.4125,
 "wakes": 37, "failedBoots": 0, "medianWakeSeconds": 142}
```

| Field | |
|---|---|
| `installId` | random UUID generated on first report, stored in `status.savings.installID`; not derived from anything about your cluster |
| `version` | nodenap version |
| `day` | UTC date of sending |
| `report` | 1, 2, 3… for this install |
| `machines` | number of Machines, bucketed: `1`, `2-3`, `4-7`, `8-15`, `16+` |
| `energySavedKWh`, `machineHoursOff`, `wakes`, `failedBoots` | cumulative totals |
| `medianWakeSeconds` | median wake time since the last report |

**Never sent:** node, machine or cluster names, IP addresses, hostnames,
locations, prices, currency or money. Money isn't comparable across
currencies, and prices can hint at where you live.

The install ID links your reports to each other (that's how the collector
checks they're consistent), and nothing else. For a new identity, delete
`status.savings.installID`. To stop, set `enabled: false` or remove the
endpoint.

## Running a collector

[`collector/`](../collector/) is a ready-made collector: a Cloudflare Worker
with a D1 database that applies the rules below and serves a page with the
totals. Its README has the deploy steps.

A collector receives reports and publishes totals. Reports are anonymous,
so anyone could send fake ones. A collector should make that ineffective
rather than try to prove identity:

1. **Plausibility.** Reject a report if:
   - a cumulative counter decreased;
   - a day's increase is physically impossible: more than `machines` (upper
     bound of the bucket) × 24 machine-hours off, or more than that × a
     generous per-machine wattage (e.g. 1 kW) in kWh;
   - `version` isn't a real release;
   - `day` is more than a day away from when it arrived.
2. **One report per install per day,** with `report` increasing by one.
   Reject duplicates and out-of-order days.
3. **Rate-limit new install IDs per source.** Hash the sender's IP address
   under a key that changes daily, keep the hash no longer than that day,
   and never store addresses. This caps how many fresh IDs one source can
   create.
4. **Count only established installs,** for example ones with seven or more
   consecutive consistent daily reports, so fake installs created in bulk
   take weeks to matter.
5. **Publish robust statistics:** totals over established installs, medians,
   per-install caps, with outliers dropped. Label the figure "self-reported by
   opted-in installs".
