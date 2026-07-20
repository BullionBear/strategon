package supervisor

import (
	"syscall"
	"time"
)

// StopSequence performs the graceful-stop timing shared by deploy DRAINING,
// strategy retirement, and agent SIGTERM:
//
//	1. call the strategy drain/cancel-orders hook, wait for it to report done
//	2. SIGTERM the process group
//	3. wait up to Grace
//	4. SIGKILL if still alive
//
// All external effects are injected so the sequence is unit-testable without
// real processes or sleeps.
type StopSequence struct {
	// Drain calls the strategy's cancel-orders/flatten endpoint. nil skips it.
	Drain func() error
	// Signal delivers sig to the process group.
	Signal func(sig syscall.Signal) error
	// Exited reports whether the process has gone.
	Exited func() bool
	// Grace is the max wait between SIGTERM and SIGKILL (stop_grace_seconds).
	Grace time.Duration
	// PollInterval is how often Exited is checked during the grace wait.
	PollInterval time.Duration
	// Sleep waits d; injectable for deterministic tests.
	Sleep func(d time.Duration)
	// Now returns current time; injectable for deterministic tests.
	Now func() time.Time
}

// Run executes the stop sequence. It returns true if the process exited before
// SIGKILL was needed.
func (s StopSequence) Run() (graceful bool) {
	if s.Drain != nil {
		_ = s.Drain()
	}
	if s.Exited() {
		return true
	}
	_ = s.Signal(syscall.SIGTERM)

	poll := s.PollInterval
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	deadline := s.Now().Add(s.Grace)
	for s.Now().Before(deadline) {
		if s.Exited() {
			return true
		}
		s.Sleep(poll)
	}
	if s.Exited() {
		return true
	}
	_ = s.Signal(syscall.SIGKILL)
	return false
}
