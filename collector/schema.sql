-- nodenap telemetry collector schema (Cloudflare D1 / SQLite).

-- One row per install: the latest raw cumulative counters (for validating
-- the next report) and the totals this collector counts toward /stats.
CREATE TABLE IF NOT EXISTS installs (
  install_id          TEXT PRIMARY KEY,
  first_day           TEXT NOT NULL,
  last_day            TEXT NOT NULL,
  last_report         INTEGER NOT NULL,
  accepted            INTEGER NOT NULL,  -- consecutive accepted reports
  machines            TEXT NOT NULL,     -- bucket: 1, 2-3, 4-7, 8-15, 16+
  version             TEXT NOT NULL,
  energy_kwh          REAL NOT NULL,     -- as reported (cumulative)
  hours_off           REAL NOT NULL,
  wakes               INTEGER NOT NULL,
  failed_boots        INTEGER NOT NULL,
  median_wake_seconds REAL NOT NULL,
  counted_energy_kwh  REAL NOT NULL,     -- capped, gap-free sum used for totals
  counted_hours_off   REAL NOT NULL,
  counted_wakes       INTEGER NOT NULL
);

-- Every accepted report, for recomputing totals or charting later.
CREATE TABLE IF NOT EXISTS reports (
  install_id          TEXT NOT NULL,
  report              INTEGER NOT NULL,
  day                 TEXT NOT NULL,
  received_at         TEXT NOT NULL,
  version             TEXT NOT NULL,
  machines            TEXT NOT NULL,
  energy_kwh          REAL NOT NULL,
  hours_off           REAL NOT NULL,
  wakes               INTEGER NOT NULL,
  failed_boots        INTEGER NOT NULL,
  median_wake_seconds REAL NOT NULL,
  counted_energy_kwh  REAL NOT NULL,
  rebaseline          INTEGER NOT NULL,
  PRIMARY KEY (install_id, report)
);
CREATE INDEX IF NOT EXISTS reports_day ON reports (day);

-- New-install rate limit: HMAC of the client IP under a per-day key.
-- Rows are deleted by the daily cron.
CREATE TABLE IF NOT EXISTS source_limits (
  day    TEXT NOT NULL,
  source TEXT NOT NULL,
  count  INTEGER NOT NULL,
  PRIMARY KEY (day, source)
);
