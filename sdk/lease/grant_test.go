package lease

import (
	"testing"
	"time"
)

func TestApplyGrantRenewPreservesJumpBaseline(t *testing.T) {
	c := &Client{
		cfg:    Config{ClockJumpThreshold: 2 * time.Second},
		margin: time.Second,
	}
	c.applyGrant("lease-1", time.Now().Add(30*time.Second))
	if c.anchor.IsZero() || c.lastWall.IsZero() {
		t.Fatal("first acquire should set anchor and lastWall")
	}
	// Simulate a prior CheckBeforeOrder sample.
	c.lastWall = c.lastWall.Add(-200 * time.Millisecond)
	c.lastElapsed = 200 * time.Millisecond
	oldAnchor := c.anchor
	oldLastWall := c.lastWall
	oldLastElapsed := c.lastElapsed

	c.applyGrant("lease-1", time.Now().Add(30*time.Second))
	if !c.anchor.Equal(oldAnchor) {
		t.Fatalf("renew reset anchor: got %v want %v", c.anchor, oldAnchor)
	}
	if !c.lastWall.Equal(oldLastWall) || c.lastElapsed != oldLastElapsed {
		t.Fatalf("renew cleared jump baseline: wall=%v elapsed=%v", c.lastWall, c.lastElapsed)
	}
	if c.deadlineMono <= oldLastElapsed {
		t.Fatalf("renew should extend deadlineMono beyond prior elapsed; got %v", c.deadlineMono)
	}
}

func TestCheckBeforeOrderDetectsJumpAfterRenew(t *testing.T) {
	c := &Client{
		cfg:    Config{ClockJumpThreshold: 50 * time.Millisecond},
		margin: time.Second,
	}
	c.applyGrant("lease-1", time.Now().Add(time.Minute))
	if err := c.CheckBeforeOrder(); err != nil {
		t.Fatal(err)
	}
	// Renew must not wipe the baseline.
	c.applyGrant("lease-1", time.Now().Add(time.Minute))
	// Fabricate a wall jump without mono advance: rewind lastWall far into the past
	// while keeping lastElapsed equal to current elapsed ⇒ large dWall, tiny dMono.
	c.mu.Lock()
	elapsed := time.Since(c.anchor)
	c.lastElapsed = elapsed
	c.lastWall = time.Now().Add(-time.Second) // 1s wall advance on next check
	c.mu.Unlock()

	if err := c.CheckBeforeOrder(); err == nil {
		t.Fatal("expected clock jump detection after renew")
	}
}
