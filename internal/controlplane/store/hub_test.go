package store

import (
	"testing"
	"time"
)

func TestSubscribeAllDeliversMachineID(t *testing.T) {
	h := NewHub()
	ch, cancel := h.SubscribeAll()
	defer cancel()
	h.Notify("m1")
	select {
	case id := <-ch:
		if id != "m1" {
			t.Fatalf("got %q, want m1", id)
		}
	case <-time.After(time.Second):
		t.Fatal("SubscribeAll did not deliver")
	}
}

func TestSubscribeAllDropsWhenSlow(t *testing.T) {
	h := NewHub()
	ch, cancel := h.SubscribeAll()
	defer cancel()
	// Fill the buffer, then a second notify must not block.
	h.Notify("m1")
	done := make(chan struct{})
	go func() {
		h.Notify("m2")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked on slow SubscribeAll")
	}
	select {
	case id := <-ch:
		if id != "m1" {
			t.Fatalf("coalesced first id = %q, want m1", id)
		}
	case <-time.After(time.Second):
		t.Fatal("expected buffered m1")
	}
}

func TestSubscribeAndSubscribeAllBothFire(t *testing.T) {
	h := NewHub()
	one, cancel1 := h.Subscribe("m1")
	defer cancel1()
	all, cancel2 := h.SubscribeAll()
	defer cancel2()
	h.Notify("m1")
	select {
	case <-one:
	case <-time.After(time.Second):
		t.Fatal("Subscribe missed")
	}
	select {
	case id := <-all:
		if id != "m1" {
			t.Fatalf("id=%q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("SubscribeAll missed")
	}
}
