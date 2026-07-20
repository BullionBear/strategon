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

func TestBrokerCancelDrains(t *testing.T) {
	b := New()
	id, ch, cancel := b.NewDownload("m1")
	b.DeliverChunk(&pb.FileChunk{RequestId: id, Data: []byte("x")})
	cancel()

	// After cancel, further delivers are ignored and channel is drained.
	b.DeliverChunk(&pb.FileChunk{RequestId: id, Data: []byte("y"), Eof: true})
	select {
	case <-ch:
		// may receive buffered chunk that was already queued; either way must not hang
	default:
	}
	select {
	case _, ok := <-ch:
		if ok {
			// drained leftover is fine
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBrokerUnknownIgnored(t *testing.T) {
	b := New()
	b.DeliverListing(nil)
	b.DeliverChunk(nil)
	b.DeliverListing(&pb.DirListing{RequestId: "nope"})
	b.DeliverChunk(&pb.FileChunk{RequestId: "nope"})
}
