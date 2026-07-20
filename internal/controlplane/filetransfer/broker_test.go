package filetransfer

import (
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestBrokerListingCorrelate(t *testing.T) {
	b := New()
	id, ch, cancel := b.NewListing("m1")
	defer cancel()

	b.DeliverListing(&pb.DirListing{RequestId: "unknown", Path: "x"}) // ignored
	b.DeliverListing(&pb.DirListing{RequestId: id, Path: "ok", Entries: []*pb.DirEntry{{Name: "a"}}})

	select {
	case listing := <-ch:
		if listing.GetPath() != "ok" || len(listing.GetEntries()) != 1 {
			t.Fatalf("unexpected listing: %+v", listing)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestBrokerCancelUnblocksDeliverChunk(t *testing.T) {
	b := New()
	id, ch, cancel := b.NewDownload("m1")

	// Fill the 32-deep buffer so the next DeliverChunk must wait on done.
	for i := 0; i < 32; i++ {
		b.DeliverChunk(&pb.FileChunk{RequestId: id, Seq: uint32(i), Data: []byte{byte(i)}})
	}

	done := make(chan struct{})
	go func() {
		b.DeliverChunk(&pb.FileChunk{RequestId: id, Seq: 32, Data: []byte("blocked")})
		close(done)
	}()

	// Give the goroutine time to block on the full channel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Unblocked by cancel — must not hang for seconds.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("DeliverChunk did not unblock after cancel")
	}

	// Drain leftover from the receive side; cancel already drained.
	select {
	case <-ch:
	default:
	}
}

func TestBrokerCancelDrains(t *testing.T) {
	b := New()
	id, ch, cancel := b.NewDownload("m1")
	b.DeliverChunk(&pb.FileChunk{RequestId: id, Data: []byte("x")})
	cancel()

	// After cancel, further delivers are ignored (pending removed).
	b.DeliverChunk(&pb.FileChunk{RequestId: id, Data: []byte("y"), Eof: true})
	select {
	case <-ch:
	default:
	}
}

func TestBrokerUnknownIgnored(t *testing.T) {
	b := New()
	b.DeliverListing(nil)
	b.DeliverChunk(nil)
	b.DeliverListing(&pb.DirListing{RequestId: "nope"})
	b.DeliverChunk(&pb.FileChunk{RequestId: "nope"})
}

func TestNewRequestIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newRequestID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
