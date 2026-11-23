package energy

import (
	"context"
	"fmt"
	"math"
	"time"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/sources"
)

// batteryDischargingWatts is the discharge rate above which the battery is
// considered to be covering load.
const batteryDischargingWatts = 50

// Snapshot is the energy situation at one moment. Prices are per kWh.
type Snapshot struct {
	Now         time.Time
	GridPrice   float64
	PriceSource string // "price", "tariff" or "none"
	ExportPrice float64
	// GridPowerWatts > 0 exporting, < 0 importing; nil when unknown.
	GridPowerWatts *float64
	BatterySoC     *float64
	BatteryUsable  bool // above reserve and (if known) discharging
	BatteryCost    *float64
	Warnings       []string
}

// importPrice is what a watt not covered by surplus costs.
func (s Snapshot) importPrice() float64 {
	if s.BatteryUsable && s.BatteryCost != nil {
		return *s.BatteryCost
	}
	return s.GridPrice
}

// CostToAddPerHour is the cost per hour of drawing watts more than now:
// surplus first (at the export price), the rest at the import price.
func (s Snapshot) CostToAddPerHour(watts float64) float64 {
	if watts <= 0 {
		return 0
	}
	fromSurplus := 0.0
	if s.GridPowerWatts != nil && *s.GridPowerWatts > 0 {
		fromSurplus = math.Min(watts, *s.GridPowerWatts)
	}
	return (fromSurplus*s.ExportPrice + (watts-fromSurplus)*s.importPrice()) / 1000
}

// SavingsToRemovePerHour is the money saved per hour by drawing watts less:
// first it reduces imports, the rest becomes export.
func (s Snapshot) SavingsToRemovePerHour(watts float64) float64 {
	if watts <= 0 {
		return 0
	}
	reducesImport := watts
	if s.GridPowerWatts != nil {
		reducesImport = math.Min(watts, math.Max(0, -*s.GridPowerWatts))
	}
	return (reducesImport*s.importPrice() + (watts-reducesImport)*s.ExportPrice) / 1000
}

// MarginalPrice is the effective price per kWh of adding watts now.
func (s Snapshot) MarginalPrice(watts float64) float64 {
	if watts <= 0 {
		return s.importPrice()
	}
	return s.CostToAddPerHour(watts) * 1000 / watts
}

// Read evaluates the energy spec at now. Problems become warnings and fall
// back (live price → tariff → 0) instead of failing the run.
func Read(ctx context.Context, spec v1alpha1.Energy, r *sources.Resolver, now time.Time) Snapshot {
	s := Snapshot{Now: now, PriceSource: "none"}
	warn := func(what string, err error) { s.Warnings = append(s.Warnings, fmt.Sprintf("%s: %v", what, err)) }
	decimal := func(what, v string) float64 {
		f, err := ParseDecimal(v, 0)
		if err != nil {
			warn(what, err)
		}
		return f
	}

	loc := time.UTC
	if spec.TimeZone != "" {
		l, err := time.LoadLocation(spec.TimeZone)
		if err != nil {
			warn("timeZone", err)
		} else {
			loc = l
		}
	}
	if spec.Price != nil {
		if v, err := r.Eval(ctx, spec.Price, ""); err != nil {
			warn("price", err)
		} else {
			s.GridPrice, s.PriceSource = v, "price"
		}
	}
	if s.PriceSource == "none" && spec.Tariff != nil {
		if t, err := NewTariff(spec.Tariff, loc); err != nil {
			warn("tariff", err)
		} else {
			s.GridPrice, s.PriceSource = t.PriceAt(now), "tariff"
		}
	}

	s.ExportPrice = decimal("exportPrice", spec.ExportPrice)
	if spec.NetMetering != nil {
		s.ExportPrice = s.GridPrice - decimal("netMetering", spec.NetMetering.NonBypassableChargesPerKWh)
	}

	if spec.GridPower != nil {
		if v, err := r.Eval(ctx, spec.GridPower, ""); err != nil {
			warn("gridPower", err)
		} else {
			s.GridPowerWatts = &v
		}
	}

	if b := spec.Battery; b != nil {
		if soc, err := r.Eval(ctx, &b.SoC, ""); err != nil {
			warn("battery.soc", err)
		} else {
			s.BatterySoC = &soc
			s.BatteryUsable = soc > float64(b.ReserveSoCPercent)
		}
		if b.Power != nil {
			if w, err := r.Eval(ctx, b.Power, ""); err != nil {
				warn("battery.power", err)
			} else if w < batteryDischargingWatts {
				s.BatteryUsable = false // charging or idle: extra load comes from the grid
			}
		}
		if b.CostPerKWh != "" {
			c := decimal("battery.costPerKWh", b.CostPerKWh)
			s.BatteryCost = &c
		}
	}
	return s
}
