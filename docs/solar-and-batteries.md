# Solar and batteries

With solar panels or a home battery, the cost of running a machine depends on
*where the next watt comes from*:

| The next watt comes from | It costs |
|---|---|
| surplus solar you'd otherwise export | the export credit you give up |
| your battery | what that stored energy would otherwise have saved you |
| the grid | the import price |

The scaler works this out each loop from two readings: grid power and the
battery.

## Grid power

`energy.gridPower` is the signed power at your meter: **positive while
exporting, negative while importing.**

```yaml
energy:
  gridPower:
    homeAssistant: {entity: sensor.grid_power}
    scale: "-1"        # if your sensor is + when importing
```

Many systems report net *consumption* (+ = importing) in kW. Flip and convert
with `scale: "-1000"`. If you only have solar production and house
consumption, compute the difference in Prometheus:

```yaml
gridPower:
  prometheus: {query: 'solar_watts - house_load_watts'}
```

Check the sign: at sunny midday with the house quiet it should be positive.

## Export price

How exported energy is credited decides whether running machines on surplus
solar is cheap:

- **Retail net metering** (e.g. older US net-metering plans): exports are
  credited at the same time-of-use price you'd pay to import, minus a few
  per-kWh charges billed only on consumption. Surplus solar is worth almost as
  much as grid power, so using it isn't free.

  ```yaml
  netMetering:
    nonBypassableChargesPerKWh: "0.012"   # per-kWh lines billed on consumption only
  ```

- **Net billing / feed-in tariffs / export guarantees** (newer US plans, UK
  SEG, most of Europe): exports earn a fixed or variable rate, usually far
  below the import price. Surplus solar is cheap to use.

  ```yaml
  exportPrice: "0.05"
  ```

## Battery

```yaml
energy:
  battery:
    soc: {homeAssistant: {entity: sensor.battery_level}}   # %
    power:                                                  # + discharging, − charging
      homeAssistant:
        entities: [sensor.battery_1_power, sensor.battery_2_power]   # summed
    reserveSoCPercent: 20
    costPerKWh: "0.55"
```

The battery is only treated as the source of extra load **while it is above
`reserveSoCPercent` and discharging** (if `power` is given). When it's charging
or idle, an extra watt doesn't come out of the battery; it comes from the grid
or from solar that would otherwise have charged it.

`costPerKWh` is what a kWh drawn from the battery is worth. If the battery
normally empties during your evening peak, every kWh the cluster takes from it
is a kWh you buy back at the peak price, so use (roughly) that price. If the
battery is usually full and you're exporting, the export reading already
covers it.

## Typical integrations

All of these are available in Home Assistant; use the entity names your
installation shows (Developer Tools → States):

| System | gridPower | battery.soc | battery.power |
|---|---|---|---|
| Enphase (Envoy) | `…_current_net_power_consumption`, kW, `scale: "-1000"` | `…_battery` | each `encharge_…_power` (W, − charging) |
| Tesla Powerwall | `…_site_power` (check sign/units) | `…_charge` | `…_battery_power` |
| SolarEdge | site/grid power sensor (check sign/units) | battery level | battery power |

See [solar-battery-net-metering.yaml](examples/solar-battery-net-metering.yaml)
for a complete config. Always confirm units (W vs kW) and sign in Home
Assistant first.

## Is it worth it?

`kubectl get powerpolicy default` shows the current `effectivePrice` next to
the import price. `scaleDown.minSavingsPerHour` skips power-offs that would
save less than that amount per hour, so with free surplus solar (exportPrice
0) machines simply stay on.
