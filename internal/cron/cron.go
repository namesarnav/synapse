// Package cron parses five-field cron expressions and computes fire times.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

type fieldSpec struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dowNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	fields     = []fieldSpec{
		{"minute", 0, 59, nil}, {"hour", 0, 23, nil}, {"day of month", 1, 31, nil},
		{"month", 1, 12, monthNames}, {"day of week", 0, 7, dowNames},
	}
	macros = map[string]string{
		"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
		"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
	}
)

// Parse parses "min hour dom month dow" or a @macro.
func Parse(spec string) (*Schedule, error) {
	spec = strings.TrimSpace(spec)
	if m, ok := macros[strings.ToLower(spec)]; ok {
		spec = m
	}
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron expression needs 5 fields (minute hour day-of-month month day-of-week), got %d", len(parts))
	}
	var bits [5]uint64
	var stars [5]bool
	for i, p := range parts {
		b, star, err := parseField(p, fields[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fields[i].name, err)
		}
		bits[i], stars[i] = b, star
	}
	// 7 is an alias for Sunday.
	if bits[4]&(1<<7) != 0 {
		bits[4] = bits[4]&^(1<<7) | 1
	}
	return &Schedule{minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4], domStar: stars[2], dowStar: stars[4]}, nil
}

func parseField(s string, f fieldSpec) (uint64, bool, error) {
	var bits uint64
	star := false
	for _, item := range strings.Split(s, ",") {
		if item == "" {
			return 0, false, fmt.Errorf("empty list item")
		}
		rng, stepStr, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, false, fmt.Errorf("invalid step %q", stepStr)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*" || rng == "?":
			if !hasStep {
				star = true
			}
			if f.name == "day of week" {
				hi = 6
			}
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = value(a, f); err != nil {
				return 0, false, err
			}
			if hi, err = value(b, f); err != nil {
				return 0, false, err
			}
			if lo > hi {
				return 0, false, fmt.Errorf("range %q is reversed", rng)
			}
		default:
			v, err := value(rng, f)
			if err != nil {
				return 0, false, err
			}
			lo, hi = v, v
			if hasStep {
				hi = f.max
			}
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, star, nil
}

func value(s string, f fieldSpec) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("value %d out of range %d-%d", n, f.min, f.max)
	}
	return n, nil
}

func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	// Like Vixie cron: when both are restricted, either may match.
	if !s.domStar && !s.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// Next returns the first fire time strictly after t in t's location, or the
// zero time if none exists within five years.
func (s *Schedule) Next(t time.Time) time.Time {
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		var n time.Time
		switch {
		case s.month&(1<<uint(t.Month())) == 0:
			n = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(t):
			n = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case s.hour&(1<<uint(t.Hour())) == 0:
			n = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case s.minute&(1<<uint(t.Minute())) == 0:
			n = t.Add(time.Minute)
		default:
			return t
		}
		// Local times inside a DST gap normalise backwards; always move forward.
		if !n.After(t) {
			n = t.Truncate(time.Hour).Add(time.Hour)
		}
		t = n
	}
	return time.Time{}
}

// Validate checks a spec and optional IANA timezone.
func Validate(spec, tz string) error {
	if _, err := Parse(spec); err != nil {
		return err
	}
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("unknown timezone %q", tz)
		}
	}
	return nil
}

// NextIn returns the next fire time after t evaluated in the named timezone.
func NextIn(spec, tz string, t time.Time) (time.Time, error) {
	s, err := Parse(spec)
	if err != nil {
		return time.Time{}, err
	}
	loc := time.UTC
	if tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return time.Time{}, fmt.Errorf("unknown timezone %q", tz)
		}
	}
	n := s.Next(t.In(loc))
	if n.IsZero() {
		return n, fmt.Errorf("cron expression never fires")
	}
	return n.UTC(), nil
}
