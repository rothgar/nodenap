// Package metrics exposes the scaler's view as Prometheus metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func gauge(name, help string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{Name: "nodenap_" + name, Help: help})
}

var (
	MachineState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nodenap_machine_state", Help: "1 for each machine's current power state.",
	}, []string{"machine", "state"})
	MachineWatts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nodenap_machine_watts", Help: "Machine power draw (source=measured|estimated).",
	}, []string{"machine", "source"})
	Price          = gauge("price_per_kwh", "Current import price per kWh.")
	EffectivePrice = gauge("effective_price_per_kwh", "Marginal price per kWh of extra load (solar/battery aware).")
	GridPower      = gauge("grid_power_watts", "Grid power; positive = exporting.")
	BatterySoC     = gauge("battery_soc_percent", "Battery state of charge.")
	MachinesWatts  = gauge("machines_watts", "Estimated draw of all machines.")
	CostPerHour    = gauge("cost_per_hour", "Estimated cost per hour of all machines.")
	PendingPods    = gauge("pending_pods", "Unschedulable pods considered for wake-ups.")
	Actions        = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nodenap_actions_total", Help: "Planner decisions (dry_run=true when not applied).",
	}, []string{"target", "dry_run"})
	Errors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nodenap_errors_total", Help: "Failed loop runs.",
	})
	EnergySaved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nodenap_energy_saved_kwh_total", Help: "Energy saved by machines being off (vs idling).",
	}, []string{"machine"})
	CostSaved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nodenap_cost_saved_total", Help: "Money saved by machines being off, at the price at the time.",
	})
	OffSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nodenap_off_seconds_total", Help: "Time machines spent fully off.",
	}, []string{"machine"})
	Wakes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nodenap_wakes_total", Help: "Machines woken and Ready.",
	})
	FailedBoots = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nodenap_failed_boots_total", Help: "Wakes where every powerOn driver timed out.",
	})
	// Lifetime totals from PowerPolicy status.savings. Unlike the counters
	// above they survive controller restarts and outlast Prometheus retention.
	LifetimeEnergy      = gauge("lifetime_energy_saved_kwh", "Energy saved since status.savings.since.")
	LifetimeCost        = gauge("lifetime_cost_saved", "Money saved since status.savings.since, at the price at the time.")
	LifetimeHoursOff    = gauge("lifetime_machine_hours_off", "Machine-hours fully off since status.savings.since.")
	LifetimeWakes       = gauge("lifetime_wakes", "Machines woken and Ready since status.savings.since.")
	LifetimeFailedBoots = gauge("lifetime_failed_boots", "Failed wakes since status.savings.since.")
	SavingsSince        = gauge("savings_since_timestamp_seconds", "When the lifetime totals started (status.savings.since).")
	WakeSeconds         = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "nodenap_wake_duration_seconds", Help: "Power-on to Node Ready.",
		Buckets: []float64{15, 30, 60, 120, 180, 300, 600, 1200},
	})
)

func init() {
	crmetrics.Registry.MustRegister(MachineState, MachineWatts, Price, EffectivePrice, GridPower,
		BatterySoC, MachinesWatts, CostPerHour, PendingPods, Actions, Errors,
		EnergySaved, CostSaved, OffSeconds, Wakes, FailedBoots, WakeSeconds,
		LifetimeEnergy, LifetimeCost, LifetimeHoursOff, LifetimeWakes, LifetimeFailedBoots, SavingsSince)
}
