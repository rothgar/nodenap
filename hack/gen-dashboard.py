#!/usr/bin/env python3
"""Generates charts/nodenap/dashboards/nodenap.json (run: python3 hack/gen-dashboard.py)."""
import json, os

DS = {"type": "prometheus", "uid": "${datasource}"}
M = 'machine=~"$machine"'
panels, pid = [], [0]

def panel(kind, title, x, y, w, h, targets, **kw):
    pid[0] += 1
    p = {"id": pid[0], "type": kind, "title": title, "datasource": DS,
         "gridPos": {"x": x, "y": y, "w": w, "h": h},
         "targets": [{"datasource": DS, "refId": chr(65 + i), "expr": e, "legendFormat": l, **({"instant": True} if kind in ("stat", "bargauge") else {})}
                     for i, (e, l) in enumerate(targets)]}
    p.update(kw)
    panels.append(p)

def row(title, y):
    pid[0] += 1
    panels.append({"id": pid[0], "type": "row", "title": title, "collapsed": False, "gridPos": {"x": 0, "y": y, "w": 24, "h": 1}, "panels": []})

def defaults(unit, decimals=None, color="fixed", fixed=None, extra=None):
    d = {"unit": unit, "color": {"mode": color, **({"fixedColor": fixed} if fixed else {})}}
    if decimals is not None:
        d["decimals"] = decimals
    d.update(extra or {})
    return {"defaults": d, "overrides": []}

stat_opts = {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
             "colorMode": "value", "graphMode": "none", "textMode": "auto", "justifyMode": "auto"}

# --- Savings -------------------------------------------------------------
row("Savings (selected time range)", 0)
panel("stat", "Money saved", 0, 1, 4, 4, [("sum(increase(nodenap_cost_saved_total[$__range]))", "")],
      fieldConfig=defaults("currencyUSD", 2, fixed="green"), options=stat_opts,
      description="Price at the time × energy not used, for machines nodenap powered off. Change the unit if you don't pay in USD.")
panel("stat", "Energy saved", 4, 1, 4, 4, [(f"sum(increase(nodenap_energy_saved_kwh_total{{{M}}}[$__range]))", "")],
      fieldConfig=defaults("kwatth", 2, fixed="green"), options=stat_opts)
panel("stat", "Machine-hours off", 8, 1, 4, 4, [(f"sum(increase(nodenap_off_seconds_total{{{M}}}[$__range])) / 3600", "")],
      fieldConfig=defaults("h", 1, fixed="blue"), options=stat_opts)
panel("stat", "Machines off now", 12, 1, 4, 4,
      [(f'count(nodenap_machine_state{{state="Off",{M}}} == 1) or vector(0)', "")],
      fieldConfig=defaults("none", 0, fixed="blue"), options=stat_opts)
panel("stat", "Wakes / failed boots", 16, 1, 4, 4,
      [("sum(increase(nodenap_wakes_total[$__range]))", "wakes"), ("sum(increase(nodenap_failed_boots_total[$__range]))", "failed")],
      fieldConfig=defaults("none", 0, color="thresholds",
                           extra={"thresholds": {"mode": "absolute", "steps": [{"color": "text", "value": None}]}}),
      options={**stat_opts, "textMode": "value_and_name"})
panel("stat", "Median wake time", 20, 1, 4, 4,
      [("histogram_quantile(0.5, sum by (le) (increase(nodenap_wake_duration_seconds_bucket[$__range])))", "")],
      fieldConfig=defaults("s", 0, fixed="text"), options=stat_opts,
      description="Power-on to Node Ready.")

# --- Now -----------------------------------------------------------------
row("Now", 5)
panel("stat", "Machines power", 0, 6, 4, 4, [("max(nodenap_machines_watts)", "")],
      fieldConfig=defaults("watt", 0, fixed="orange"), options={**stat_opts, "graphMode": "area"})
panel("stat", "Machines cost / hour", 4, 6, 4, 4, [("max(nodenap_cost_per_hour)", "")],
      fieldConfig=defaults("currencyUSD", 3, fixed="orange"), options={**stat_opts, "graphMode": "area"})
panel("stat", "Grid price / kWh", 8, 6, 4, 4, [("max(nodenap_price_per_kwh)", "")],
      fieldConfig=defaults("currencyUSD", 4, fixed="text"), options=stat_opts)
panel("stat", "Effective price / kWh", 12, 6, 4, 4, [("max(nodenap_effective_price_per_kwh)", "")],
      fieldConfig=defaults("currencyUSD", 4, fixed="text"), options=stat_opts,
      description="What 100 W more load would cost per kWh after solar export and battery.")
panel("stat", "Pending pods", 16, 6, 4, 4, [("max(nodenap_pending_pods)", "")],
      fieldConfig=defaults("none", 0, color="thresholds",
                           extra={"thresholds": {"mode": "absolute", "steps": [{"color": "green", "value": None}, {"color": "yellow", "value": 1}]}}),
      options=stat_opts)
panel("stat", "Would act (dry run)", 20, 6, 4, 4,
      [('sum by (target) (increase(nodenap_actions_total{dry_run="true"}[$__range]))', "{{target}}")],
      fieldConfig=defaults("none", 0, fixed="purple"), options={**stat_opts, "textMode": "value_and_name"},
      description="Decisions nodenap logged but did not apply because dryRun is true.")

# --- Machines ------------------------------------------------------------
row("Machines", 10)
states = [("Off", 1, "#808080"), ("Booting", 2, "blue"), ("On", 3, "green"),
          ("Draining", 4, "yellow"), ("ShuttingDown", 5, "orange"), ("Failed", 6, "red"), ("Unknown", 0, "purple")]
expr = "max by (machine) (\n" + "\n  or ".join(
    f'(nodenap_machine_state{{state="{s}",{M}}} == 1) * {v}' for s, v, _ in states) + "\n)"
panel("state-timeline", "Power state", 0, 11, 24, 8, [(expr, "{{machine}}")],
      # Fixed color, not thresholds: in thresholds mode the state timeline buckets values by threshold
      # and ignores the per-state colors and names in the mappings below.
      fieldConfig={"defaults": {"color": {"mode": "fixed", "fixedColor": "purple"}, "custom": {"fillOpacity": 80, "lineWidth": 0},
                                "mappings": [{"type": "value", "options": {str(v): {"text": s, "color": c, "index": i}
                                                                           for i, (s, v, c) in enumerate(states)}}]},
                   "overrides": []},
      options={"showValue": "never", "mergeValues": True, "rowHeight": 0.8, "alignValue": "left",
               "legend": {"showLegend": True, "displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "single"}})
ts_custom = {"drawStyle": "line", "lineWidth": 1, "fillOpacity": 10, "showPoints": "never", "spanNulls": False}
panel("timeseries", "Power by machine", 0, 19, 12, 8,
      [(f"max by (machine) (nodenap_machine_watts{{{M}}})", "{{machine}}")],
      fieldConfig=defaults("watt", 0, color="palette-classic", extra={"custom": {**ts_custom, "stacking": {"mode": "normal"}}}),
      options={"legend": {"showLegend": True, "displayMode": "table", "placement": "right", "calcs": ["mean", "lastNotNull"]},
               "tooltip": {"mode": "multi", "sort": "desc"}},
      description="Measured draw (sensor) of each machine; machines without a sensor are absent.")
panel("bargauge", "Energy saved by machine", 12, 19, 6, 8,
      [(f"sort_desc(sum by (machine) (increase(nodenap_energy_saved_kwh_total{{{M}}}[$__range])))", "{{machine}}")],
      fieldConfig=defaults("kwatth", 2, fixed="green"),
      options={"orientation": "horizontal", "displayMode": "gradient", "showUnfilled": True,
               "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}})
panel("bargauge", "Hours off by machine", 18, 19, 6, 8,
      [(f"sort_desc(sum by (machine) (increase(nodenap_off_seconds_total{{{M}}}[$__range])) / 3600)", "{{machine}}")],
      fieldConfig=defaults("h", 1, fixed="blue"),
      options={"orientation": "horizontal", "displayMode": "gradient", "showUnfilled": True,
               "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}})

# --- Energy --------------------------------------------------------------
row("Energy", 27)
panel("timeseries", "Price per kWh", 0, 28, 8, 8,
      [("max(nodenap_price_per_kwh)", "grid price"), ("max(nodenap_effective_price_per_kwh)", "effective (solar/battery)")],
      fieldConfig=defaults("currencyUSD", 4, color="palette-classic", extra={"custom": {**ts_custom, "drawStyle": "line", "lineInterpolation": "stepAfter"}}),
      options={"legend": {"showLegend": True, "displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}})
panel("timeseries", "Savings over time", 8, 28, 8, 8,
      [("sum(increase(nodenap_cost_saved_total[$__interval]))", "saved")],
      fieldConfig=defaults("currencyUSD", 3, fixed="green", extra={"custom": {**ts_custom, "drawStyle": "bars", "fillOpacity": 60}}),
      options={"legend": {"showLegend": False}, "tooltip": {"mode": "single"}})
panel("timeseries", "Grid and battery", 16, 28, 8, 8,
      [("max(nodenap_grid_power_watts)", "grid (+ export)"), ("max(nodenap_battery_soc_percent)", "battery SoC")],
      fieldConfig={"defaults": {"unit": "watt", "color": {"mode": "palette-classic"}, "custom": ts_custom},
                   "overrides": [{"matcher": {"id": "byName", "options": "battery SoC"},
                                  "properties": [{"id": "unit", "value": "percent"}, {"id": "min", "value": 0}, {"id": "max", "value": 100},
                                                 {"id": "custom.axisPlacement", "value": "right"}]}]},
      options={"legend": {"showLegend": True, "displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}},
      description="Only present when the PowerPolicy sets energy.gridPower / energy.battery.")

dash = {
    "title": "nodenap", "uid": "nodenap", "tags": ["nodenap", "power"], "timezone": "browser",
    "schemaVersion": 39, "version": 1, "editable": True, "graphTooltip": 1, "refresh": "1m",
    "time": {"from": "now-7d", "to": "now"},
    "description": "Machine power states, draw, and the energy and money nodenap saved. https://github.com/rothgar/nodenap",
    "links": [{"title": "nodenap docs", "type": "link", "url": "https://github.com/rothgar/nodenap/tree/main/docs", "targetBlank": True}],
    "templating": {"list": [
        {"name": "datasource", "label": "Data source", "type": "datasource", "query": "prometheus", "current": {}, "hide": 0},
        {"name": "machine", "label": "Machine", "type": "query", "datasource": DS,
         "query": {"query": "label_values(nodenap_machine_state, machine)", "refId": "machine"},
         "definition": "label_values(nodenap_machine_state, machine)",
         "multi": True, "includeAll": True, "allValue": ".*", "current": {"text": "All", "value": "$__all"}, "refresh": 2, "sort": 1},
    ]},
    "annotations": {"list": []},
    "panels": panels,
}
out = os.path.join(os.path.dirname(__file__), "..", "charts", "nodenap", "dashboards", "nodenap.json")
with open(out, "w") as f:
    json.dump(dash, f, indent=2)
    f.write("\n")
