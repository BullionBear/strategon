// Package clock provides a Clock abstraction so that time-dependent logic
// (backoff, health windows, lease TTLs, resync ticks) can be driven by a fake
// clock in tests without sleeping real time.
package clock

import "time"

// Clock is the minimal time surface the reconciler and supervisor depend on.
type Clock interface {
	Now() time.Time
	// Ticker returns a ticker firing on the returned channel every d. The
	// returned Ticker must be stopped by the caller.
	Ticker(d time.Duration) Ticker
}

// Ticker mirrors the parts of time.Ticker used by the reconciler.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Real is a Clock backed by the wall clock.
type Real struct{}

// Now returns the current wall-clock time.
func (Real) Now() time.Time { return time.Now() }

// Ticker returns a real time-backed ticker.
func (Real) Ticker(d time.Duration) Ticker { return &realTicker{t: time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }
