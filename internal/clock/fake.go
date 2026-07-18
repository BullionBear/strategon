package clock

import (
	"sync"
	"time"
)

// Fake is a deterministic Clock for tests. Advance() moves time forward and
// fires any registered tickers whose interval elapsed, allowing backoff,
// health windows, and lease TTLs to be fast-forwarded without real sleeps.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

// NewFake returns a Fake clock seeded at start.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now returns the current fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Ticker registers a fake ticker firing every d of advanced time.
func (f *Fake) Ticker(d time.Duration) Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTicker{c: make(chan time.Time, 1), interval: d, next: f.now.Add(d)}
	f.tickers = append(f.tickers, t)
	return t
}

// Advance moves the clock forward by d, firing every registered ticker once for
// each interval boundary crossed (coalesced to one pending tick per ticker,
// matching time.Ticker's buffered-channel drop semantics).
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	f.now = target
	var toFire []*fakeTicker
	for _, t := range f.tickers {
		if t.stopped {
			continue
		}
		fired := false
		for !t.next.After(target) {
			t.next = t.next.Add(t.interval)
			fired = true
		}
		if fired {
			toFire = append(toFire, t)
		}
	}
	f.mu.Unlock()

	for _, t := range toFire {
		select {
		case t.c <- target:
		default:
		}
	}
}

type fakeTicker struct {
	c        chan time.Time
	interval time.Duration
	next     time.Time
	stopped  bool
}

func (t *fakeTicker) C() <-chan time.Time { return t.c }
func (t *fakeTicker) Stop()               { t.stopped = true }
