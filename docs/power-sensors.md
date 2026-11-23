# Power sensors

A machine's `sensor` tells the scaler how many watts it draws while it's on.
The scaler uses it to:

- compare machines when deciding which to power off (most watts per core goes
  first),
- report cost (`kubectl get machines`, metrics),
- estimate what powering a machine off would save.

Without a sensor, the scaler uses the machine's `profile`: `idleWatts`, plus
`wattsPerCore` × requested cores, capped at `maxWatts`. **The profile is
always used for machines that are off** (to estimate what waking them
costs), so set `idleWatts` even when you have a sensor.

**Measure every machine the same way if you can, at the wall.** Mixing a
CPU-only estimate for one machine with wall power for another makes the
CPU-only one look several times cheaper than it is.

| Source | Measures | Notes |
|---|---|---|
| Smart plug (Shelly, Tasmota, Kasa, ...) | wall power | best; also gives you power-on/cut |
| BMC (IPMI DCMI, Redfish) | wall/PSU power | servers |
| Prometheus | whatever you have | node exporters, plug exporters, GPU exporters |
| Kepler | **CPU package only** (RAPL) | Intel/AMD only, under-reports badly; see below |
| Home Assistant | whatever HA knows | via `http` with a token |

## Smart plugs

See the `http` examples in [power drivers](power-drivers.md): Shelly Gen2+
(`field: apower`), Shelly Gen1 (`field: meters.0.power`), Tasmota
(`field: StatusSNS.ENERGY.Power`).

A plug's own draw is ~0.5–1 W. That isn't worth it for a 4 W Raspberry Pi,
but is for anything bigger. One plug on a shared USB power supply can measure
several Pis together.

## Prometheus

```yaml
sensor:
  prometheus:
    query: 'shelly_power_watts{device="shelly-{{ .NodeName }}"}'
```

`{{ .NodeName }}` is replaced with the Machine's node name. Set
`spec.integrations.prometheusURL` on the PowerPolicy. The query's samples are
summed, so make sure it returns one series per machine.

Exporter restarts can briefly leave two series for the same device. Wrap
those in `max(...)` instead of letting them be summed:

```yaml
query: 'max(nvidia_smi_power_draw_watts{instance="{{ .NodeName }}"})'
```

### Kepler

[Kepler](https://sustainable-computing.io/) estimates power from CPU energy
counters (Intel/AMD RAPL). That covers the CPU package and sometimes DRAM,
**not** the GPU, disks, fans, board or power-supply losses, and there is no
data on ARM machines without RAPL. Use it as part of an estimate:

```yaml
sensor:
  prometheus:
    query: >-
      sum(kepler_node_cpu_watts{node_name="{{ .NodeName }}",zone="package"})
      + max(nvidia_smi_power_draw_watts{instance="{{ .NodeName }}"})
      + 30
```

Calibrate the constant (here 30 W for board, disks, fans and PSU losses)
once with a plug. **Don't put `#` comments inside a query:** PromQL treats `#`
as a comment, and a YAML `>-` block is folded onto one line, so everything
after it would be ignored.

## IPMI and Redfish

```yaml
sensor:
  redfish: {address: https://192.168.10.21, credentialsSecretRef: {name: idrac}, insecureSkipTLSVerify: true}
```

```yaml
sensor:
  ipmi: {address: 192.168.10.31, credentialsSecretRef: {name: bmc}}
```

BMC readings usually include everything behind the PSU. Some BMCs keep
drawing 5–15 W while the machine is "off"; put that in `profile.offWatts`.

## Home Assistant

```yaml
sensor:
  http:
    url: http://homeassistant.home:8123/api/states/sensor.nas_power
    field: state
    scale: "1000"     # if the sensor reports kW
    headersSecretRef: {name: ha-headers}   # key Authorization: "Bearer <token>"
```

## Measuring hardware that isn't a Kubernetes node

The scaler only manages Kubernetes nodes. To graph the rest of your homelab
(a NAS, switches, the router), scrape their plugs into Prometheus directly
(for example with a JSON exporter for Shelly devices). Multiply by
`nodenap_price_per_kwh` in Grafana to show cost; see
[how it works](how-it-works.md#metrics).
