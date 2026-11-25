// Fills [data-stat] elements with the public totals from the nodenap
// telemetry collector (CORS-open, cached 30s), and refreshes them every
// minute while the page is visible. Elements:
//   data-stat="energy|hours|installs|wakes|wake|after|status|days"
const STATS_URL = "https://nodenap-collector.123dev.workers.dev/stats";

(() => {
  const fmt = (n, d = 0) => n.toLocaleString(undefined, { maximumFractionDigits: d, minimumFractionDigits: d });
  const energy = (kwh) =>
    kwh >= 1e6 ? `${fmt(kwh / 1e6, 2)} GWh` : kwh >= 1e3 ? `${fmt(kwh / 1e3, 2)} MWh` : `${fmt(kwh, kwh < 100 ? 1 : 0)} kWh`;
  const duration = (s) =>
    !s ? "–" : s < 120 ? `${Math.round(s)}s` : `${Math.floor(s / 60)}m ${String(Math.round(s % 60)).padStart(2, "0")}s`;

  const render = {
    energy: (s) => energy(s.energySavedKWh),
    hours: (s) => fmt(s.machineHoursOff),
    days: (s) => fmt(s.machineHoursOff / 24),
    installs: (s) => fmt(s.installs),
    wakes: (s) => fmt(s.wakes),
    wake: (s) => duration(s.medianWakeSeconds),
    after: (s) => String(s.establishedAfterReports),
    status: (s) => {
      const parts = [`${fmt(s.activeInstalls)} installs reported this week`];
      if (s.pendingInstalls) parts.push(`${fmt(s.pendingInstalls)} more in their first ${s.establishedAfterReports} days`);
      parts.push(`updated ${new Date(s.updatedAt).toLocaleTimeString()}`);
      return parts.join(" · ");
    },
  };

  async function load() {
    const els = document.querySelectorAll("[data-stat]");
    if (!els.length) return;
    try {
      const res = await fetch(STATS_URL, { headers: { accept: "application/json" } });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const s = await res.json();
      els.forEach((el) => {
        const f = render[el.dataset.stat];
        if (f) el.textContent = f(s);
      });
    } catch (err) {
      document.querySelectorAll('[data-stat="status"]').forEach((el) => {
        el.textContent = `Couldn't load the totals right now (${err.message}).`;
      });
    }
  }

  let timer;
  const start = () => { load(); clearInterval(timer); timer = setInterval(() => !document.hidden && load(), 60_000); };
  // Material's instant navigation swaps pages without a reload.
  if (window.document$) window.document$.subscribe(start);
  else document.addEventListener("DOMContentLoaded", start);
})();
