// Package ratelimit provides in-memory token buckets keyed by string.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a set of token buckets refilled at Rate tokens per second up to
// Burst. It is safe for concurrent use.
type Limiter struct {
	Rate  float64
	Burst int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func New(rate float64, burst int) *Limiter {
	return &Limiter{Rate: rate, Burst: burst, now: time.Now, buckets: map[string]*bucket{}}
}

// WithClock overrides the clock (tests).
func (l *Limiter) WithClock(now func() time.Time) *Limiter { l.now = now; return l }

// Allow takes one token for key, returning false and the wait until the next
// token when the bucket is empty.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	return l.AllowN(key, 1)
}

func (l *Limiter) AllowN(key string, n int) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(now)
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(l.Burst), last: now}
		l.buckets[key] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens += el * l.Rate
		if b.tokens > float64(l.Burst) {
			b.tokens = float64(l.Burst)
		}
		b.last = now
	}
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return true, 0
	}
	if l.Rate <= 0 {
		return false, time.Hour
	}
	need := float64(n) - b.tokens
	return false, time.Duration(need / l.Rate * float64(time.Second))
}

// gc drops idle full buckets so memory stays bounded under many keys.
func (l *Limiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	for k, b := range l.buckets {
		idle := now.Sub(b.last).Seconds()
		if l.Rate > 0 && b.tokens+idle*l.Rate >= float64(l.Burst) {
			delete(l.buckets, k)
		}
	}
}

// Len returns the number of tracked keys.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
