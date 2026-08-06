package ratelimit

import (
	"testing"
	"time"
)

func TestBurstThenRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(2, 3).WithClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("burst request %d denied", i)
		}
	}
	ok, wait := l.Allow("k")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("expected denial with 500ms wait, got ok=%v wait=%v", ok, wait)
	}
	now = now.Add(time.Second) // refills 2 tokens
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("expected allow after refill")
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("expected second allow after refill")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("bucket should be empty again")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(0.001, 1)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a first")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b first")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a second should be denied")
	}
}

func TestRefillCapsAtBurst(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(100, 2).WithClock(func() time.Time { return now })
	l.Allow("k")
	now = now.Add(time.Hour)
	l.Allow("k")
	l.Allow("k")
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("cap at burst violated")
	}
}

func TestGCDropsIdleBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(10, 5).WithClock(func() time.Time { return now })
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	now = now.Add(2 * time.Minute)
	l.Allow("d")
	if n := l.Len(); n != 1 {
		t.Fatalf("expected idle buckets collected, have %d", n)
	}
}

func TestZeroRateNeverRefills(t *testing.T) {
	l := New(0, 1)
	l.Allow("k")
	if ok, w := l.Allow("k"); ok || w < time.Minute {
		t.Fatalf("ok=%v wait=%v", ok, w)
	}
}
