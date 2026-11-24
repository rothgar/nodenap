// Plausibility checks for nodenap's anonymous daily telemetry report.
// Pure functions, no Workers APIs, so they can be unit tested with node:test.
// The rules follow docs/savings-and-telemetry.md ("Running a collector").

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const DAY = /^\d{4}-\d{2}-\d{2}$/;
const DAY_MS = 86_400_000;

// Upper bound of each machine-count bucket the controller sends.
export const MACHINE_BUCKETS = { "1": 1, "2-3": 3, "4-7": 7, "8-15": 15, "16+": 64 };

export const DEFAULTS = {
  maxWattsPerMachine: 1000, // a generous idle draw for one machine
  firstReportMaxDays: 90, // how much history a first report may claim
  maxMedianWakeSeconds: 3600,
};

const isCount = (v) => Number.isFinite(v) && v >= 0;

export function dayNumber(day) {
  return Math.floor(Date.parse(`${day}T00:00:00Z`) / DAY_MS);
}

// parseReport checks the shape of a decoded JSON body and returns a clean copy,
// or throws with a reason.
export function parseReport(body) {
  if (!body || typeof body !== "object") throw new Error("body must be a JSON object");
  const r = {
    installId: body.installId,
    version: body.version,
    day: body.day,
    report: body.report,
    machines: body.machines,
    energySavedKWh: body.energySavedKWh,
    machineHoursOff: body.machineHoursOff,
    wakes: body.wakes,
    failedBoots: body.failedBoots,
    medianWakeSeconds: body.medianWakeSeconds ?? 0,
  };
  if (typeof r.installId !== "string" || !UUID.test(r.installId)) throw new Error("installId must be a lowercase UUID");
  if (typeof r.version !== "string" || r.version.length > 64) throw new Error("bad version");
  if (typeof r.day !== "string" || !DAY.test(r.day) || Number.isNaN(dayNumber(r.day))) throw new Error("day must be YYYY-MM-DD");
  if (!Number.isInteger(r.report) || r.report < 1) throw new Error("report must be a positive integer");
  if (!(r.machines in MACHINE_BUCKETS)) throw new Error("unknown machines bucket");
  for (const k of ["energySavedKWh", "machineHoursOff", "wakes", "failedBoots", "medianWakeSeconds"]) {
    if (!isCount(r[k])) throw new Error(`${k} must be a non-negative number`);
  }
  if (!Number.isInteger(r.wakes) || !Number.isInteger(r.failedBoots)) throw new Error("wakes and failedBoots must be integers");
  if (r.medianWakeSeconds > DEFAULTS.maxMedianWakeSeconds) throw new Error("medianWakeSeconds out of range");
  return r;
}

// evaluate decides what to do with a parsed report given the install's stored
// state (null for a new install). It returns:
//   {action: "reject", status, reason}
//   {action: "duplicate"}                     already have this report number
//   {action: "accept", counted: {...}, accepted, rebaseline}
// counted is how much to ADD to the install's counted totals.
export function evaluate(r, prev, nowMs, opts = {}) {
  const o = { ...DEFAULTS, ...opts };
  const maxKW = o.maxWattsPerMachine / 1000;
  const machines = MACHINE_BUCKETS[r.machines];

  const today = Math.floor(nowMs / DAY_MS);
  const day = dayNumber(r.day);
  if (Math.abs(day - today) > 1) return reject(422, "day is more than a day away from now");

  if (!prev) {
    // A first report carries everything saved before telemetry was turned
    // on. Accept it, but count at most firstReportMaxDays of it.
    const hoursCap = machines * 24 * o.firstReportMaxDays;
    const hours = Math.min(r.machineHoursOff, hoursCap);
    const energy = Math.min(r.energySavedKWh, hours * maxKW);
    const wakes = Math.min(r.wakes, machines * 96 * o.firstReportMaxDays);
    return { action: "accept", counted: { energy, hours, wakes }, accepted: 1, rebaseline: false };
  }

  if (r.report <= prev.last_report) return { action: "duplicate" };

  if (r.energySavedKWh < prev.energy_kwh || r.machineHoursOff < prev.hours_off || r.wakes < prev.wakes || r.failedBoots < prev.failed_boots) {
    return reject(422, "a cumulative counter decreased");
  }

  const prevDay = dayNumber(prev.last_day);
  if (day <= prevDay) return reject(409, "already have a report for this day or a later one");

  if (r.report !== prev.last_report + 1) {
    // Reports were accepted somewhere else (or lost). Take the new values as
    // a fresh baseline: count nothing for the gap and restart the wait
    // before this install counts as established.
    return { action: "accept", counted: { energy: 0, hours: 0, wakes: 0 }, accepted: 1, rebaseline: true };
  }

  // Reports are >= 24h apart but land on calendar days, so allow one extra day.
  const days = day - prevDay + 1;
  const dHours = r.machineHoursOff - prev.hours_off;
  const dEnergy = r.energySavedKWh - prev.energy_kwh;
  const dWakes = r.wakes - prev.wakes;
  if (dHours > machines * 24 * days + 1e-6) return reject(422, "more machine-hours off than the machines could have been off");
  if (dEnergy > dHours * maxKW + 1e-3) return reject(422, `more than ${o.maxWattsPerMachine} W saved per machine-hour off`);
  if (dWakes > machines * 96 * days) return reject(422, "implausible number of wakes");

  return { action: "accept", counted: { energy: dEnergy, hours: dHours, wakes: dWakes }, accepted: prev.accepted + 1, rebaseline: false };
}

function reject(status, reason) {
  return { action: "reject", status, reason };
}
