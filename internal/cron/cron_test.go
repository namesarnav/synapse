package cron

import (
	"testing"
	"time"
)

func mustNext(t *testing.T, spec string, from time.Time) time.Time {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("%q: %v", spec, err)
	}
	return s.Next(from)
}

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	cases := []struct{ spec, from, want string }{
		{"* * * * *", "2026-01-01 10:00", "2026-01-01 10:01"},
		{"*/15 * * * *", "2026-01-01 10:07", "2026-01-01 10:15"},
		{"0 9 * * *", "2026-01-01 09:00", "2026-01-02 09:00"},
		{"30 2 * * *", "2026-03-01 03:00", "2026-03-02 02:30"},
		{"0 0 1 * *", "2026-01-31 12:00", "2026-02-01 00:00"},
		{"0 0 29 2 *", "2026-01-01 00:00", "2028-02-29 00:00"},
		{"0 12 * * mon-fri", "2026-01-02 12:00", "2026-01-05 12:00"}, // Fri -> Mon
		{"0 12 * * 7", "2026-01-01 00:00", "2026-01-04 12:00"},       // 7 = Sunday
		{"0,30 8-10 * * *", "2026-01-01 08:30", "2026-01-01 09:00"},
		{"@hourly", "2026-01-01 10:30", "2026-01-01 11:00"},
		{"@weekly", "2026-01-01 00:00", "2026-01-04 00:00"},
		{"0 0 13 * fri", "2026-01-01 00:00", "2026-01-02 00:00"}, // dom OR dow
		{"5/20 * * * *", "2026-01-01 10:06", "2026-01-01 10:25"},
		{"0 0 * jan,jul *", "2026-02-01 00:00", "2026-07-01 00:00"},
	}
	for _, c := range cases {
		if got := mustNext(t, c.spec, ts(c.from)); !got.Equal(ts(c.want)) {
			t.Errorf("Next(%q, %s) = %s, want %s", c.spec, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, spec := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "*/0 * * * *", "5-2 * * * *", "a * * * *", "1,,2 * * * *", "* * * * 8"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) should fail", spec)
		}
	}
}

func TestNeverFires(t *testing.T) {
	s, err := Parse("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Next(ts("2026-01-01 00:00")); !n.IsZero() {
		t.Fatalf("Feb 31 fired at %s", n)
	}
	if _, err := NextIn("0 0 31 2 *", "UTC", ts("2026-01-01 00:00")); err == nil {
		t.Fatal("NextIn should report a never-firing schedule")
	}
}

func TestTimezoneAndDST(t *testing.T) {
	// 09:00 New York is 14:00 UTC in January (EST) and 13:00 UTC in July (EDT).
	got, err := NextIn("0 9 * * *", "America/New_York", ts("2026-01-15 00:00"))
	if err != nil || !got.Equal(ts("2026-01-15 14:00")) {
		t.Fatalf("winter: %v %v", got, err)
	}
	got, _ = NextIn("0 9 * * *", "America/New_York", ts("2026-07-15 00:00"))
	if !got.Equal(ts("2026-07-15 13:00")) {
		t.Fatalf("summer: %v", got)
	}
	if err := Validate("* * * * *", "Mars/Olympus"); err == nil {
		t.Fatal("unknown timezone accepted")
	}
	// The nonexistent 02:30 on the spring-forward day still yields a later time, never a loop.
	got, err = NextIn("30 2 * * *", "America/New_York", ts("2026-03-08 00:00"))
	if err != nil || !got.After(ts("2026-03-08 00:00")) {
		t.Fatalf("dst gap: %v %v", got, err)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"* * * * *", "*/5 1-3 * jan mon", "@daily", "0 0 1,15 * *"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		sc, err := Parse(s)
		if err == nil {
			_ = sc.Next(time.Unix(1_700_000_000, 0).UTC())
		}
	})
}
