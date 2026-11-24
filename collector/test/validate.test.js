import { test } from "node:test";
import assert from "node:assert/strict";
import { evaluate, parseReport } from "../src/validate.js";

const NOW = Date.parse("2026-11-24T12:00:00Z");
const base = {
  installId: "6a1f0c7e-3b9d-4f44-9a0e-2d1c5b7e8f90",
  version: "v0.1.6",
  day: "2026-11-24",
  report: 2,
  machines: "4-7",
  energySavedKWh: 50,
  machineHoursOff: 800,
  wakes: 40,
  failedBoots: 0,
  medianWakeSeconds: 142,
};
const prev = {
  last_day: "2026-11-23",
  last_report: 1,
  accepted: 1,
  energy_kwh: 41.2,
  hours_off: 763.4,
  wakes: 37,
  failed_boots: 0,
};
const r = (over = {}) => parseReport({ ...base, ...over });

test("parseReport rejects bad shapes", () => {
  assert.throws(() => parseReport(null));
  assert.throws(() => r({ installId: "not-a-uuid" }));
  assert.throws(() => r({ day: "24/11/2026" }));
  assert.throws(() => r({ report: 0 }));
  assert.throws(() => r({ machines: "5" }));
  assert.throws(() => r({ energySavedKWh: -1 }));
  assert.throws(() => r({ wakes: 1.5 }));
  assert.throws(() => r({ medianWakeSeconds: 99999 }));
  assert.equal(r().report, 2);
});

test("accepts the next consistent report and counts the delta", () => {
  const v = evaluate(r(), prev, NOW);
  assert.equal(v.action, "accept");
  assert.equal(v.accepted, 2);
  assert.ok(Math.abs(v.counted.energy - 8.8) < 1e-9);
  assert.ok(Math.abs(v.counted.hours - 36.6) < 1e-9);
  assert.equal(v.counted.wakes, 3);
});

test("day must be near now", () => {
  assert.equal(evaluate(r({ day: "2026-11-20" }), prev, NOW).status, 422);
  assert.equal(evaluate(r({ day: "2026-11-25" }), prev, NOW).action, "accept");
});

test("a counter that goes down is rejected", () => {
  assert.equal(evaluate(r({ energySavedKWh: 40 }), prev, NOW).status, 422);
  assert.equal(evaluate(r({ wakes: 36 }), prev, NOW).status, 422);
});

test("same report number is a duplicate, same day is a conflict", () => {
  assert.equal(evaluate(r({ report: 1 }), prev, NOW).action, "duplicate");
  assert.equal(evaluate(r({ day: "2026-11-23" }), prev, NOW).status, 409);
});

test("impossible increases are rejected", () => {
  // 4-7 bucket: at most 7 machines x 24h x (1 day + 1 slack) = 336h.
  assert.equal(evaluate(r({ machineHoursOff: 763.4 + 337 }), prev, NOW).status, 422);
  assert.equal(evaluate(r({ machineHoursOff: 763.4 + 330, energySavedKWh: 41.2 + 330 }), prev, NOW).action, "accept");
  // More than 1 kW per machine-hour off.
  assert.equal(evaluate(r({ energySavedKWh: 41.2 + 37 }), prev, NOW).status, 422);
});

test("skipped report numbers rebaseline without counting the gap", () => {
  const v = evaluate(r({ report: 5 }), { ...prev, accepted: 9 }, NOW);
  assert.equal(v.action, "accept");
  assert.equal(v.rebaseline, true);
  assert.equal(v.accepted, 1);
  assert.deepEqual(v.counted, { energy: 0, hours: 0, wakes: 0 });
});

test("first report is accepted but capped", () => {
  const v = evaluate(r({ report: 1, machines: "1", machineHoursOff: 1e6, energySavedKWh: 1e6 }), null, NOW);
  assert.equal(v.action, "accept");
  assert.equal(v.counted.hours, 24 * 90);
  assert.equal(v.counted.energy, 24 * 90 * 1);
  const small = evaluate(r({ report: 1 }), null, NOW);
  assert.equal(small.counted.energy, 50);
});
