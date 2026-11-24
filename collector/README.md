# nodenap telemetry collector

A Cloudflare Worker that receives nodenap's opt-in anonymous daily reports,
stores them in D1, and serves a public page with the totals: energy saved,
machine-hours off and wakes.

| Path | |
|---|---|
| `POST /report` | a controller's daily report ([format](../docs/savings-and-telemetry.md#anonymous-telemetry-opt-in)) |
| `GET /stats` | totals as JSON, cached for 30 seconds, CORS open so the page can be hosted anywhere |
| `/` | the stats page, from `public/`; it checks `/stats` every 30 seconds and counts up to new totals |

## What it accepts

Reports are anonymous, so the collector doesn't try to prove who sent one.
Instead it makes fake ones ineffective
([rules](../docs/savings-and-telemetry.md#running-a-collector)):

- `version` must be a tag of `REPO`; `day` within a day of now.
- One report per install per day, report numbers increasing by one. A
  repeated report number gets `200` (so a controller that missed the
  response moves on). A skipped number starts the install's count over.
- Cumulative counters can't go down. A day's increase can't exceed the
  machine bucket's upper bound × 24 h off, or `MAX_WATTS_PER_MACHINE` for
  every machine-hour off.
- A first report counts at most `FIRST_REPORT_MAX_DAYS` of earlier savings.
- At most `NEW_INSTALLS_PER_SOURCE_PER_DAY` new install IDs per source. The
  source is an HMAC of the client IP under a key that changes daily; the
  daily cron deletes them. IP addresses are never stored.
- `/stats` totals only installs with `ESTABLISHED_REPORTS` consistent
  reports in a row.

Controllers retry every hour after a non-2xx response, so rejections are
cheap but not silent: the controller logs the status and reason.

## Deploy

```sh
cd collector
npm install
npx wrangler login
npx wrangler d1 create nodenap-telemetry       # put the id in wrangler.toml
npm run db:init                                # create the tables
openssl rand -hex 32 | npx wrangler secret put IP_HASH_SECRET
npm run deploy
```

Then point controllers at it:

```yaml
spec:
  telemetry:
    enabled: true
    endpoint: https://nodenap-collector.<your-subdomain>.workers.dev/report
```

Settings are the `[vars]` in `wrangler.toml`.

## Develop

```sh
npm test                                        # validation rules
echo 'IP_HASH_SECRET=dev' > .dev.vars
npm run db:init:local
npx wrangler dev --var ESTABLISHED_REPORTS:1    # http://localhost:8787
```

```sh
curl -H 'content-type: application/json' localhost:8787/report -d '{
  "installId": "6a1f0c7e-3b9d-4f44-9a0e-2d1c5b7e8f90", "version": "v0.1.6",
  "day": "'"$(date -u +%F)"'", "report": 1, "machines": "4-7",
  "energySavedKWh": 41.2, "machineHoursOff": 763.4, "wakes": 37,
  "failedBoots": 0, "medianWakeSeconds": 142}'
curl localhost:8787/stats
```
