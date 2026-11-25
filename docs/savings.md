---
title: Public savings
hide: [navigation, toc]
---

# Public savings

The energy nodenap has saved across every install that opted in to the
anonymous daily report. The totals refresh every minute.

<div class="nn-tiles">
  <div class="nn-tile nn-big"><div class="nn-label">Energy saved</div><div class="nn-value" data-stat="energy">–</div></div>
  <div class="nn-tile"><div class="nn-label">Machine-hours off</div><div class="nn-value" data-stat="hours">–</div></div>
  <div class="nn-tile"><div class="nn-label">Machine-days off</div><div class="nn-value" data-stat="days">–</div></div>
  <div class="nn-tile"><div class="nn-label">Wakes</div><div class="nn-value" data-stat="wakes">–</div></div>
  <div class="nn-tile"><div class="nn-label">Median wake</div><div class="nn-value" data-stat="wake">–</div></div>
</div>
<p class="nn-status" data-stat="status">Loading…</p>

These figures are self-reported by opted-in installs. An install counts
after <span data-stat="after">3</span> consistent daily reports. Reports
contain energy saved, machine-hours off and wakes, and never names,
addresses, prices or money. Raw totals:
[`/stats`](https://nodenap-collector.123dev.workers.dev/stats).

## Add yours

Turn on the report in your PowerPolicy. It's one small JSON payload a day,
logged before it's sent ([what's sent](savings-and-telemetry.md#anonymous-telemetry-opt-in)).

```yaml
spec:
  telemetry:
    enabled: true
    endpoint: https://nodenap-collector.123dev.workers.dev/report
```
