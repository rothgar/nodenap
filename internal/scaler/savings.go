package scaler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/energy"
	"github.com/rothgar/nodenap/internal/metrics"
)

// Version is reported in telemetry; set at build time with
// -ldflags "-X github.com/rothgar/nodenap/internal/scaler.Version=v0.1.0".
var Version = "dev"

// tally is what one loop run adds to the savings totals.
type tally struct {
	kWh, cost, hoursOff float64
	wakes, failedBoots  int64
}

func (s *Scaler) recordWake(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.run.wakes++
	s.wakeSeconds = append(s.wakeSeconds, d.Seconds())
	metrics.Wakes.Inc()
	metrics.WakeSeconds.Observe(d.Seconds())
}

func (s *Scaler) recordFailedBoot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.run.failedBoots++
	metrics.FailedBoots.Inc()
}

// accountOff credits a machine that the scaler powered off (poweredOffAt) and
// is fully Off, for dt. Machines found already off, or observed in dry run,
// aren't savings the scaler caused. The baseline is what it would have drawn
// idling: its last reading before it went off, or its profile. Draining,
// shutdown and boot are never credited.
func (s *Scaler) accountOff(m *v1alpha1.Machine, e energy.Snapshot, dt time.Duration) {
	if m.Status.PowerState != v1alpha1.PowerOff || m.Status.PoweredOffAt == nil || dt <= 0 {
		return
	}
	p := profile(m)
	baseline := p.IdleWatts
	if w, err := energy.ParseDecimal(m.Status.MeasuredWatts, -1); err == nil && w > 0 {
		baseline = w
	}
	saved := baseline - p.OffWatts
	if saved <= 0 {
		return
	}
	h := dt.Hours()
	kWh := saved * h / 1000
	cost := e.SavingsToRemovePerHour(saved) * h
	s.mu.Lock()
	s.run.kWh += kWh
	s.run.cost += cost
	s.run.hoursOff += h
	s.mu.Unlock()
	metrics.EnergySaved.WithLabelValues(m.Name).Add(kWh)
	metrics.CostSaved.Add(cost)
	metrics.OffSeconds.WithLabelValues(m.Name).Add(dt.Seconds())
}

// addSavings folds this run's tally into the persisted totals.
func (s *Scaler) addSavings(sv *v1alpha1.Savings, now time.Time) {
	s.mu.Lock()
	t := s.run
	s.run = tally{}
	s.mu.Unlock()
	add := func(cur string, d float64, prec int) string {
		v, _ := energy.ParseDecimal(cur, 0)
		return strconv.FormatFloat(v+d, 'f', prec, 64)
	}
	if sv.Since == nil {
		sv.Since = &metav1.Time{Time: now}
	}
	// Each 15s loop adds tiny amounts (a 2 W saving is ~8e-6 kWh), so keep
	// enough precision that rounding the stored total never swallows them.
	sv.EnergyKWh = add(sv.EnergyKWh, t.kWh, 6)
	sv.Cost = add(sv.Cost, t.cost, 6)
	sv.MachineHoursOff = add(sv.MachineHoursOff, t.hoursOff, 6)
	sv.Wakes += t.wakes
	sv.FailedBoots += t.failedBoots
}

// Report is the anonymous daily telemetry payload: cumulative counters only.
// The collector validates it against the same install's previous reports.
type Report struct {
	InstallID         string  `json:"installId"`
	Version           string  `json:"version"`
	Day               string  `json:"day"`    // UTC date of sending
	Report            int64   `json:"report"` // 1, 2, 3, ... per install
	Machines          string  `json:"machines"`
	EnergySavedKWh    float64 `json:"energySavedKWh"`  // cumulative
	MachineHoursOff   float64 `json:"machineHoursOff"` // cumulative
	Wakes             int64   `json:"wakes"`           // cumulative
	FailedBoots       int64   `json:"failedBoots"`     // cumulative
	MedianWakeSeconds float64 `json:"medianWakeSeconds"`
}

// machineBucket hides exact fleet sizes.
func machineBucket(n int) string {
	switch {
	case n <= 1:
		return "1"
	case n <= 3:
		return "2-3"
	case n <= 7:
		return "4-7"
	case n <= 15:
		return "8-15"
	default:
		return "16+"
	}
}

const (
	reportEvery = 24 * time.Hour
	retryEvery  = time.Hour
)

// maybeReport sends the daily report when telemetry is enabled and due.
func (s *Scaler) maybeReport(ctx context.Context, t v1alpha1.Telemetry, sv *v1alpha1.Savings, machines int, now time.Time) {
	if !t.Enabled || t.Endpoint == "" {
		return
	}
	if sv.InstallID == "" {
		sv.InstallID = uuid.NewString()
	}
	if sv.LastReportTime != nil && now.Sub(sv.LastReportTime.Time) < reportEvery {
		return
	}
	s.mu.Lock()
	if now.Sub(s.lastReportAttempt) < retryEvery {
		s.mu.Unlock()
		return
	}
	s.lastReportAttempt = now
	samples := append([]float64(nil), s.wakeSeconds...)
	s.mu.Unlock()

	num := func(v string) float64 { f, _ := energy.ParseDecimal(v, 0); return f }
	r := Report{
		InstallID: sv.InstallID, Version: Version, Day: now.UTC().Format("2006-01-02"),
		Report: sv.Reports + 1, Machines: machineBucket(machines),
		EnergySavedKWh: num(sv.EnergyKWh), MachineHoursOff: num(sv.MachineHoursOff),
		Wakes: sv.Wakes, FailedBoots: sv.FailedBoots, MedianWakeSeconds: median(samples),
	}
	body, _ := json.Marshal(r)
	logger := log.FromContext(ctx)
	logger.Info("sending anonymous telemetry report (spec.telemetry.enabled=true)", "endpoint", t.Endpoint, "payload", string(body))

	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		logger.Error(err, "telemetry request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Error(err, "telemetry report failed; retrying within an hour")
		return
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		logger.Error(fmt.Errorf("HTTP %d", resp.StatusCode), "telemetry report rejected; retrying within an hour")
		return
	}
	sv.LastReportTime, sv.Reports = &metav1.Time{Time: now}, r.Report
	s.mu.Lock()
	s.wakeSeconds = nil
	s.mu.Unlock()
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	m := len(v) / 2
	if len(v)%2 == 1 {
		return v[m]
	}
	return (v[m-1] + v[m]) / 2
}
