package supervisor

import (
	"syscall"
	"testing"
	"time"
)

func TestExpBackoffCap(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{7, 64 * time.Second},
		{100, 64 * time.Second}, // capped
	}
	for _, c := range cases {
		if got := ExpBackoff(c.n, nil); got != c.want {
			t.Errorf("ExpBackoff(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

func TestBackoffStateProgression(t *testing.T) {
	now := time.Unix(1000, 0)
	var b BackoffState
	b.RecordCrash(now, nil)
	if b.Consecutive != 1 || !b.Blocked(now) {
		t.Fatalf("after 1 crash: consec=%d blocked=%v", b.Consecutive, b.Blocked(now))
	}
	if b.Blocked(now.Add(2 * time.Second)) {
		t.Fatalf("should be unblocked after 1s backoff")
	}
	b.RecordCrash(now, nil) // 2nd crash -> 2s
	if !b.Blocked(now.Add(1 * time.Second)) {
		t.Fatalf("2nd crash should block for 2s")
	}
	b.Reset()
	if b.Consecutive != 0 || b.Blocked(now) {
		t.Fatalf("reset should clear state")
	}
}

func TestCrashedOnStart(t *testing.T) {
	if !CrashedOnStart(500*time.Millisecond, 5) {
		t.Fatalf("500ms < 5s startsecs should be a crash")
	}
	if CrashedOnStart(10*time.Second, 5) {
		t.Fatalf("10s >= 5s startsecs should not be a crash")
	}
}

func TestStopSequenceGracefulExit(t *testing.T) {
	var sigs []syscall.Signal
	now := time.Unix(0, 0)
	exited := false
	seq := StopSequence{
		Drain:  func() error { return nil },
		Signal: func(s syscall.Signal) error { sigs = append(sigs, s); return nil },
		// Exit right after SIGTERM (drain-then-check sees alive, post-SIGTERM sees gone).
		Exited: func() bool { return exited },
		Grace:  5 * time.Second,
		Sleep:  func(d time.Duration) { now = now.Add(d); exited = true },
		Now:    func() time.Time { return now },
	}
	if graceful := seq.Run(); !graceful {
		t.Fatalf("expected graceful stop")
	}
	if len(sigs) != 1 || sigs[0] != syscall.SIGTERM {
		t.Fatalf("expected only SIGTERM, got %v", sigs)
	}
}

func TestStopSequenceForcesKill(t *testing.T) {
	var sigs []syscall.Signal
	now := time.Unix(0, 0)
	seq := StopSequence{
		Signal:       func(s syscall.Signal) error { sigs = append(sigs, s); return nil },
		Exited:       func() bool { return false }, // never exits gracefully
		Grace:        1 * time.Second,
		PollInterval: 250 * time.Millisecond,
		Sleep:        func(d time.Duration) { now = now.Add(d) },
		Now:          func() time.Time { return now },
	}
	if graceful := seq.Run(); graceful {
		t.Fatalf("expected forced kill")
	}
	if len(sigs) != 2 || sigs[0] != syscall.SIGTERM || sigs[1] != syscall.SIGKILL {
		t.Fatalf("expected SIGTERM then SIGKILL, got %v", sigs)
	}
}
