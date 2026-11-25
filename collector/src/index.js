// nodenap telemetry collector: a Cloudflare Worker with a D1 database.
//
//   POST /report  one anonymous daily report from a controller
//   GET  /stats   public totals over established installs (JSON, CORS, 30s cache)
//   everything else is the static page in ../public
//
// See docs/savings-and-telemetry.md for what controllers send and the rules
// applied here.

import { evaluate, parseReport } from "./validate.js";

const MAX_BODY = 4096;

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    try {
      if (url.pathname === "/report") {
        if (request.method !== "POST") return text(405, "POST a report");
        return await handleReport(request, env);
      }
      if (url.pathname === "/stats") {
        if (request.method === "OPTIONS") return new Response(null, { status: 204, headers: cors() });
        if (request.method !== "GET" && request.method !== "HEAD") return text(405, "GET only");
        return await handleStats(request, env, ctx);
      }
    } catch (err) {
      console.error(err);
      return text(500, "internal error");
    }
    return env.ASSETS.fetch(request);
  },

  // Daily: forget yesterday's hashed sources.
  async scheduled(_event, env) {
    await env.DB.prepare("DELETE FROM source_limits WHERE day < ?").bind(utcDay(Date.now())).run();
  },
};

async function handleReport(request, env) {
  const len = Number(request.headers.get("content-length") || 0);
  if (len > MAX_BODY) return text(413, "report too large");
  const raw = await request.text();
  if (raw.length > MAX_BODY) return text(413, "report too large");

  let r;
  try {
    r = parseReport(JSON.parse(raw));
  } catch (err) {
    return text(400, err.message);
  }

  const releases = await releaseTags(env);
  if (!releases) return text(503, "can't check the version right now; try later");
  if (!releases.has(r.version)) return text(422, "version isn't a nodenap release");

  const now = Date.now();
  const prev = await env.DB.prepare("SELECT * FROM installs WHERE install_id = ?").bind(r.installId).first();

  if (!prev) {
    const limited = await overNewInstallLimit(request, env, now);
    if (limited) return text(429, "too many new installs from this source today");
  }

  const v = evaluate(r, prev, now, {
    maxWattsPerMachine: num(env.MAX_WATTS_PER_MACHINE, 1000),
    firstReportMaxDays: num(env.FIRST_REPORT_MAX_DAYS, 90),
  });
  if (v.action === "reject") return text(v.status, v.reason);
  if (v.action === "duplicate") return text(200, "already have this report");

  const c = v.counted;
  const stmts = [];
  if (!prev) {
    stmts.push(
      env.DB.prepare(
        `INSERT INTO installs (install_id, first_day, last_day, last_report, accepted, machines, version,
           energy_kwh, hours_off, wakes, failed_boots, median_wake_seconds,
           counted_energy_kwh, counted_hours_off, counted_wakes)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      ).bind(r.installId, r.day, r.day, r.report, v.accepted, r.machines, r.version,
        r.energySavedKWh, r.machineHoursOff, r.wakes, r.failedBoots, r.medianWakeSeconds,
        c.energy, c.hours, c.wakes),
    );
  } else {
    stmts.push(
      env.DB.prepare(
        `UPDATE installs SET last_day = ?, last_report = ?, accepted = ?, machines = ?, version = ?,
           energy_kwh = ?, hours_off = ?, wakes = ?, failed_boots = ?, median_wake_seconds = ?,
           counted_energy_kwh = counted_energy_kwh + ?, counted_hours_off = counted_hours_off + ?,
           counted_wakes = counted_wakes + ?
         WHERE install_id = ? AND last_report = ?`,
      ).bind(r.day, r.report, v.accepted, r.machines, r.version,
        r.energySavedKWh, r.machineHoursOff, r.wakes, r.failedBoots, r.medianWakeSeconds,
        c.energy, c.hours, c.wakes, r.installId, prev.last_report),
    );
  }
  stmts.push(
    env.DB.prepare(
      `INSERT OR IGNORE INTO reports (install_id, report, day, received_at, version, machines,
         energy_kwh, hours_off, wakes, failed_boots, median_wake_seconds, counted_energy_kwh, rebaseline)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    ).bind(r.installId, r.report, r.day, new Date(now).toISOString(), r.version, r.machines,
      r.energySavedKWh, r.machineHoursOff, r.wakes, r.failedBoots, r.medianWakeSeconds, c.energy, v.rebaseline ? 1 : 0),
  );
  const [res] = await env.DB.batch(stmts);
  // Lost a race with a concurrent report for the same install.
  if (prev && res.meta.changes === 0) return text(409, "conflicting report; try later");
  // Let the next /stats in this location see the new totals right away.
  await caches.default.delete(new Request(new URL("/stats", request.url).toString()));
  return text(202, v.rebaseline ? "accepted (counting restarts: report numbers skipped)" : "accepted");
}

async function handleStats(request, env, ctx) {
  const cache = caches.default;
  const key = new Request(new URL("/stats", request.url).toString());
  const hit = await cache.match(key);
  if (hit) return hit;

  const established = num(env.ESTABLISHED_REPORTS, 3);
  const today = utcDay(Date.now());
  const week = utcDay(Date.now() - 7 * 86_400_000);
  const row = await env.DB.prepare(
    `SELECT COUNT(*) AS installs,
            COALESCE(SUM(counted_energy_kwh), 0) AS energy,
            COALESCE(SUM(counted_hours_off), 0) AS hours,
            COALESCE(SUM(counted_wakes), 0) AS wakes,
            COALESCE(SUM(CASE WHEN last_day >= ? THEN 1 ELSE 0 END), 0) AS active
       FROM installs WHERE accepted >= ?`,
  ).bind(week, established).first();
  const pending = await env.DB.prepare("SELECT COUNT(*) AS n FROM installs WHERE accepted < ? AND last_day >= ?")
    .bind(established, week).first();
  const wakeRows = await env.DB.prepare(
    "SELECT median_wake_seconds AS s FROM installs WHERE accepted >= ? AND last_day >= ? AND median_wake_seconds > 0",
  ).bind(established, week).all();

  const body = {
    installs: row.installs,
    activeInstalls: row.active,
    pendingInstalls: pending.n,
    establishedAfterReports: established,
    energySavedKWh: round(row.energy, 3),
    machineHoursOff: round(row.hours, 1),
    wakes: row.wakes,
    medianWakeSeconds: median(wakeRows.results.map((x) => x.s)),
    day: today,
    updatedAt: new Date().toISOString(),
  };
  const resp = new Response(JSON.stringify(body), {
    headers: { "content-type": "application/json", "cache-control": "public, max-age=30", ...cors() },
  });
  ctx.waitUntil(cache.put(key, resp.clone()));
  return resp;
}

// releaseTags returns the set of published nodenap tags, cached for an hour,
// or null when GitHub can't be reached and nothing is cached.
async function releaseTags(env) {
  const repo = env.REPO || "rothgar/nodenap";
  const cache = caches.default;
  const key = new Request(`https://nodenap-collector.internal/tags/${repo}`);
  let resp = await cache.match(key);
  if (!resp) {
    const gh = await fetch(`https://api.github.com/repos/${repo}/tags?per_page=100`, {
      headers: { "user-agent": "nodenap-collector", accept: "application/vnd.github+json" },
    });
    if (!gh.ok) return null;
    const names = (await gh.json()).map((t) => t.name);
    resp = new Response(JSON.stringify(names), { headers: { "cache-control": "max-age=3600" } });
    await cache.put(key, resp.clone());
  }
  return new Set(await resp.json());
}

// overNewInstallLimit counts new install IDs per source per UTC day. The
// source is an HMAC of the client IP under a key that changes every day, so
// stored values can't be matched to an address or across days, and the cron
// deletes them the next day. Addresses are never stored.
async function overNewInstallLimit(request, env, now) {
  const limit = num(env.NEW_INSTALLS_PER_SOURCE_PER_DAY, 3);
  const ip = request.headers.get("cf-connecting-ip") || "unknown";
  const day = utcDay(now);
  const source = await hmac(`${env.IP_HASH_SECRET || ""}:${day}`, ip);
  const row = await env.DB.prepare(
    `INSERT INTO source_limits (day, source, count) VALUES (?, ?, 1)
     ON CONFLICT(day, source) DO UPDATE SET count = count + 1
     RETURNING count`,
  ).bind(day, source).first();
  return row.count > limit;
}

async function hmac(secret, msg) {
  const enc = new TextEncoder();
  const k = await crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = new Uint8Array(await crypto.subtle.sign("HMAC", k, enc.encode(msg)));
  return [...sig.slice(0, 16)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

function median(v) {
  if (!v.length) return 0;
  v.sort((a, b) => a - b);
  const m = v.length >> 1;
  return round(v.length % 2 ? v[m] : (v[m - 1] + v[m]) / 2, 1);
}

const utcDay = (ms) => new Date(ms).toISOString().slice(0, 10);
const round = (x, d) => Math.round(x * 10 ** d) / 10 ** d;
const num = (v, d) => (v === undefined || v === "" || Number.isNaN(Number(v)) ? d : Number(v));
const cors = () => ({ "access-control-allow-origin": "*", "access-control-allow-methods": "GET, HEAD, OPTIONS" });
const text = (status, msg) => new Response(msg + "\n", { status, headers: { "content-type": "text/plain" } });
