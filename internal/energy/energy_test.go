package energy

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/sources"
)

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s: got %v want %v", name, got, want)
	}
}

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// The shape of a US time-of-use plan with summer and winter seasons.
func TestSeasonalTariff(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	tr, err := NewTariff(&v1alpha1.Tariff{DefaultPrice: "0.39", Windows: []v1alpha1.TariffWindow{
		{Months: []int32{6, 7, 8, 9}, Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri"}, Start: "17:00", End: "20:00", Price: "0.75"},
		{Months: []int32{6, 7, 8, 9}, Start: "17:00", End: "20:00", Price: "0.55"},
		{Months: []int32{6, 7, 8, 9}, Start: "00:00", End: "00:00", Price: "0.35"},
		{Start: "17:00", End: "20:00", Price: "0.57"},
		{Start: "08:00", End: "17:00", Price: "0.33"},
		{Start: "23:00", End: "02:00", Price: "0.30", Days: []string{"Sat"}},
	}}, loc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		when string
		want float64
	}{
		{"2026-07-15 18:00", 0.75}, // summer weekday peak
		{"2026-07-18 18:00", 0.55}, // summer weekend
		{"2026-07-15 03:00", 0.35}, // summer whole-day window
		{"2026-11-20 18:00", 0.57}, // winter peak
		{"2026-11-20 12:00", 0.33}, // winter midday
		{"2026-11-20 21:00", 0.39}, // default
		{"2026-11-22 01:00", 0.30}, // wraps midnight, belongs to Saturday's window
		{"2026-11-23 01:00", 0.39}, // Monday 01:00 is not Sunday-night's window
	} {
		approx(t, c.when, tr.PriceAt(at(t, loc, c.when)), c.want)
	}
	approx(t, "UTC input", tr.PriceAt(at(t, loc, "2026-11-20 18:00").UTC()), 0.57)
	if _, err := NewTariff(&v1alpha1.Tariff{DefaultPrice: "1", Windows: []v1alpha1.TariffWindow{{Months: []int32{13}, Start: "00:00", End: "01:00", Price: "1"}}}, loc); err == nil {
		t.Fatal("month 13 must fail")
	}
}

func TestMarginalPricing(t *testing.T) {
	exp := 500.0
	s := Snapshot{GridPrice: 0.30, ExportPrice: 0.05, GridPowerWatts: &exp}
	approx(t, "within surplus", s.CostToAddPerHour(200), 0.2*0.05)
	approx(t, "beyond surplus", s.CostToAddPerHour(800), 0.5*0.05+0.3*0.30)
	approx(t, "remove while exporting", s.SavingsToRemovePerHour(100), 0.1*0.05)
	imp := -300.0
	s = Snapshot{GridPrice: 0.30, ExportPrice: 0.05, GridPowerWatts: &imp}
	approx(t, "remove while importing", s.SavingsToRemovePerHour(500), 0.3*0.30+0.2*0.05)
	s = Snapshot{GridPrice: 0.25}
	approx(t, "no meter data: grid price", s.SavingsToRemovePerHour(400), 0.1)
}

func TestReadWithHomeAssistant(t *testing.T) {
	vals := map[string]string{"price": "0.42", "grid": "1.09", "soc": "43", "b1": "-455", "b2": "-450"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"state":%q}`, vals[r.URL.Path[len("/api/states/"):]])
	}))
	defer srv.Close()
	r := &sources.Resolver{HA: sources.NewHomeAssistant(srv.URL, "tok")}
	ha := func(ids ...string) *v1alpha1.Value {
		return &v1alpha1.Value{HomeAssistant: &v1alpha1.HomeAssistantSource{Entities: ids}}
	}
	spec := v1alpha1.Energy{
		Tariff:      &v1alpha1.Tariff{DefaultPrice: "0.33"},
		Price:       ha("price"),
		NetMetering: &v1alpha1.NetMetering{NonBypassableChargesPerKWh: "0.02"},
		GridPower:   &v1alpha1.Value{HomeAssistant: &v1alpha1.HomeAssistantSource{Entity: "grid"}, Scale: "-1000"},
		Battery:     &v1alpha1.Battery{SoC: *ha("soc"), Power: ha("b1", "b2"), ReserveSoCPercent: 40, CostPerKWh: "0.57"},
	}
	s := Read(context.Background(), spec, r, time.Now())
	if len(s.Warnings) > 0 {
		t.Fatal(s.Warnings)
	}
	approx(t, "live price wins over tariff", s.GridPrice, 0.42)
	approx(t, "net metering export", s.ExportPrice, 0.40)
	approx(t, "kW import → signed W", *s.GridPowerWatts, -1090) // net consumption: import is + kW; scale -1000 flips it
	// Importing, battery above reserve but charging: extra load is grid-priced.
	approx(t, "charging battery not used", s.MarginalPrice(100), 0.42)
	vals["b1"], vals["b2"] = "600", "600"
	s = Read(context.Background(), spec, r, time.Now())
	approx(t, "discharging battery used", s.MarginalPrice(100), 0.57)

	// A broken live price falls back to the tariff, with a warning.
	delete(vals, "price")
	s = Read(context.Background(), spec, r, time.Now())
	approx(t, "fallback to tariff", s.GridPrice, 0.33)
	if s.PriceSource != "tariff" || len(s.Warnings) == 0 {
		t.Fatalf("source %s warnings %v", s.PriceSource, s.Warnings)
	}
}
