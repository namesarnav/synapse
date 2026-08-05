package workflow

import (
	"math"
	"time"
)

// RetryPolicy configures exponential backoff with jitter.
type RetryPolicy struct {
	MaxAttempts    int     `json:"max_attempts"`     // total attempts including the first; <=1 means no retry
	InitialDelayMS int     `json:"initial_delay_ms"` // delay before the 2nd attempt
	MaxDelayMS     int     `json:"max_delay_ms"`
	Multiplier     float64 `json:"multiplier"`
	Jitter         float64 `json:"jitter"` // fraction in [0,1]; delay is scaled by 1±jitter
}

// DefaultRetry is applied to nodes without an explicit policy.
var DefaultRetry = RetryPolicy{MaxAttempts: 1}

func (p *RetryPolicy) attempts() int {
	if p == nil || p.MaxAttempts < 1 {
		return 1
	}
	return p.MaxAttempts
}

// CanRetry reports whether another attempt is allowed after `attempt` (1-based) failed.
func (p *RetryPolicy) CanRetry(attempt int) bool { return attempt < p.attempts() }

// Delay returns the wait before the attempt following `attempt` (1-based).
// rnd returns a uniform value in [0,1); it is injectable for tests.
func (p *RetryPolicy) Delay(attempt int, rnd func() float64) time.Duration {
	if p == nil {
		return 0
	}
	initial := float64(p.InitialDelayMS)
	if initial <= 0 {
		initial = 1000
	}
	mult := p.Multiplier
	if mult < 1 {
		mult = 2
	}
	d := initial * math.Pow(mult, float64(attempt-1))
	if p.MaxDelayMS > 0 && d > float64(p.MaxDelayMS) {
		d = float64(p.MaxDelayMS)
	}
	if p.Jitter > 0 && rnd != nil {
		j := math.Min(p.Jitter, 1)
		d *= 1 + j*(2*rnd()-1)
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d * float64(time.Millisecond))
}

// Validate checks bounds of the policy.
func (p *RetryPolicy) Validate() error {
	if p == nil {
		return nil
	}
	switch {
	case p.MaxAttempts < 0 || p.MaxAttempts > 100:
		return errString("retry.max_attempts must be between 1 and 100")
	case p.InitialDelayMS < 0 || p.MaxDelayMS < 0:
		return errString("retry delays must be non-negative")
	case p.MaxDelayMS > 0 && p.InitialDelayMS > p.MaxDelayMS:
		return errString("retry.initial_delay_ms must not exceed max_delay_ms")
	case p.Multiplier != 0 && p.Multiplier < 1:
		return errString("retry.multiplier must be >= 1")
	case p.Jitter < 0 || p.Jitter > 1:
		return errString("retry.jitter must be in [0,1]")
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }
