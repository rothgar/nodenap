// Package energy works out what electricity costs right now, accounting for
// time-of-use tariffs, dynamic prices, solar surplus and batteries.
package energy

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
)

// Tariff is a parsed static time-of-use tariff.
type Tariff struct {
	Default float64
	Windows []window
	Loc     *time.Location
}

type window struct {
	days       map[time.Weekday]bool // nil = every day
	months     map[time.Month]bool   // nil = every month
	start, end int                   // minutes since midnight
	price      float64
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func parseHHMM(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("time %q is not HH:MM", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("time %q is not HH:MM", s)
	}
	return hh*60 + mm, nil
}

// ParseDecimal parses an optional decimal string; empty yields def.
func ParseDecimal(s string, def float64) (float64, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

// NewTariff parses the API spec.
func NewTariff(spec *v1alpha1.Tariff, loc *time.Location) (*Tariff, error) {
	if spec == nil {
		return nil, nil
	}
	def, err := ParseDecimal(spec.DefaultPrice, 0)
	if err != nil {
		return nil, fmt.Errorf("tariff defaultPrice: %w", err)
	}
	t := &Tariff{Default: def, Loc: loc}
	for i, w := range spec.Windows {
		start, err := parseHHMM(w.Start)
		if err != nil {
			return nil, fmt.Errorf("tariff window %d: %w", i, err)
		}
		end, err := parseHHMM(w.End)
		if err != nil {
			return nil, fmt.Errorf("tariff window %d: %w", i, err)
		}
		price, err := ParseDecimal(w.Price, 0)
		if err != nil {
			return nil, fmt.Errorf("tariff window %d price: %w", i, err)
		}
		pw := window{start: start, end: end, price: price}
		if len(w.Months) > 0 {
			pw.months = map[time.Month]bool{}
			for _, m := range w.Months {
				if m < 1 || m > 12 {
					return nil, fmt.Errorf("tariff window %d: month %d out of range", i, m)
				}
				pw.months[time.Month(m)] = true
			}
		}
		if len(w.Days) > 0 {
			pw.days = map[time.Weekday]bool{}
			for _, d := range w.Days {
				wd, ok := weekdays[strings.ToLower(d)[:min(3, len(d))]]
				if !ok {
					return nil, fmt.Errorf("tariff window %d: unknown day %q", i, d)
				}
				pw.days[wd] = true
			}
		}
		t.Windows = append(t.Windows, pw)
	}
	return t, nil
}

// PriceAt returns the price per kWh at t.
func (t *Tariff) PriceAt(at time.Time) float64 {
	local := at.In(t.Loc)
	minute := local.Hour()*60 + local.Minute()
	for _, w := range t.Windows {
		if w.matches(local, minute) {
			return w.price
		}
	}
	return t.Default
}

func (w window) matches(local time.Time, minute int) bool {
	if w.start == w.end { // 00:00-00:00: the whole day
		return w.dayOK(local)
	}
	if w.start < w.end {
		return w.dayOK(local) && minute >= w.start && minute < w.end
	}
	// Wraps midnight: the part after midnight belongs to the previous day's window.
	if minute >= w.start {
		return w.dayOK(local)
	}
	if minute < w.end {
		return w.dayOK(local.AddDate(0, 0, -1))
	}
	return false
}

func (w window) dayOK(t time.Time) bool {
	return (w.days == nil || w.days[t.Weekday()]) && (w.months == nil || w.months[t.Month()])
}
