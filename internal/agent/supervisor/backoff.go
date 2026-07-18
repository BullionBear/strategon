// Package supervisor holds the process-supervision primitives borrowed from
// supervisord (RECONCILER.md §6.3, §7): startsecs classification, exponential
// crash backoff with a cap, and the graceful-stop signal sequence. These are
// kept as small, pure, injectable pieces so the reconciler can drive them and
// tests can fast-forward without real sleeps.
package supervisor

import (
	"math/rand"
	"time"
)

const (
	// baseBackoff is the first backoff step; subsequent steps double up to the
	// cap.
	baseBackoff = time.Second
	// maxBackoffShift caps the doubling at 1s<<6 = 64s (RECONCILER §6.3).
	maxBackoffShift = 6
)

// BackoffState tracks consecutive crashes and when a restart is next allowed.
type BackoffState struct {
	Consecutive  int
	BlockedUntil time.Time
}

// Blocked reports whether restarts are currently held off.
func (b BackoffState) Blocked(now time.Time) bool {
	return b.BlockedUntil.After(now)
}

// Reset clears the crash counter (process lived long enough to be "started").
func (b *BackoffState) Reset() {
	b.Consecutive = 0
	b.BlockedUntil = time.Time{}
}

// RecordCrash increments the crash counter and schedules the next allowed
// restart using exponential backoff. jitterFn may be nil for deterministic
// tests.
func (b *BackoffState) RecordCrash(now time.Time, jitterFn func(time.Duration) time.Duration) {
	b.Consecutive++
	b.BlockedUntil = now.Add(ExpBackoff(b.Consecutive, jitterFn))
}

// ExpBackoff returns 1s,2s,4s,...,64s (capped) plus optional jitter of up to
// d/4. jitterFn is injectable; pass nil to disable jitter (tests).
func ExpBackoff(n int, jitterFn func(time.Duration) time.Duration) time.Duration {
	shift := n - 1
	if shift < 0 {
		shift = 0
	}
	if shift > maxBackoffShift {
		shift = maxBackoffShift
	}
	d := baseBackoff << uint(shift)
	if jitterFn != nil {
		d += jitterFn(d / 4)
	}
	return d
}

// DefaultJitter returns a uniformly-random duration in [0, max).
func DefaultJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

// CrashedOnStart reports whether a process that lived for `lived` before exiting
// should be counted as a startup failure (crash) rather than a clean run.
// A process must survive at least startsecs to count as successfully started;
// otherwise a fast crash loop would be mistaken for N successful restarts and
// mask a bad version (RECONCILER §6.3).
func CrashedOnStart(lived time.Duration, startsecs int) bool {
	return lived < time.Duration(startsecs)*time.Second
}
