// Package filetransfer correlates browse/fetch requests over the agent bidi
// stream with waiting human-API handlers.
package filetransfer

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

const (
	// BrowseTimeout is how long BrowseDir waits for a DirListing.
	BrowseTimeout = 30 * time.Second
	// DownloadTimeout is how long DownloadFiles may run.
	DownloadTimeout = 5 * time.Minute
)

// Pending is a registered in-flight browse or download request.
type Pending struct {
	MachineID string
	ListingCh chan *pb.DirListing
	ChunkCh   chan *pb.FileChunk
	cancel    func()
}

// Broker maps request_id → pending waiter. Shared by the human API (producer)
// and the grpcstream server (consumer of agent responses).
type Broker struct {
	mu   sync.Mutex
	reqs map[string]*Pending
}

// New returns an empty Broker.
func New() *Broker {
	return &Broker{reqs: map[string]*Pending{}}
}

// NewListing registers a browse waiter. cancel removes the entry and drains.
func (b *Broker) NewListing(machineID string) (requestID string, listingCh <-chan *pb.DirListing, cancel func()) {
	id := newRequestID()
	ch := make(chan *pb.DirListing, 1)
	p := &Pending{MachineID: machineID, ListingCh: ch}
	var once sync.Once
	cancel = func() {
		once.Do(func() {
			b.remove(id)
			drainListing(ch)
		})
	}
	p.cancel = cancel
	b.mu.Lock()
	b.reqs[id] = p
	b.mu.Unlock()
	return id, ch, cancel
}

// NewDownload registers a download waiter. cancel removes the entry and drains.
func (b *Broker) NewDownload(machineID string) (requestID string, chunkCh <-chan *pb.FileChunk, cancel func()) {
	id := newRequestID()
	ch := make(chan *pb.FileChunk, 32)
	p := &Pending{MachineID: machineID, ChunkCh: ch}
	var once sync.Once
	cancel = func() {
		once.Do(func() {
			b.remove(id)
			drainChunks(ch)
		})
	}
	p.cancel = cancel
	b.mu.Lock()
	b.reqs[id] = p
	b.mu.Unlock()
	return id, ch, cancel
}

// DeliverListing completes a browse waiter. Unknown request_ids are ignored.
func (b *Broker) DeliverListing(listing *pb.DirListing) {
	if listing == nil {
		return
	}
	b.mu.Lock()
	p := b.reqs[listing.GetRequestId()]
	b.mu.Unlock()
	if p == nil || p.ListingCh == nil {
		return
	}
	select {
	case p.ListingCh <- listing:
	default:
		// Already delivered or cancelled.
	}
}

// DeliverChunk forwards a file chunk. Unknown request_ids are ignored.
// The channel may block briefly under backpressure; callers should not hold
// locks across this call.
func (b *Broker) DeliverChunk(chunk *pb.FileChunk) {
	if chunk == nil {
		return
	}
	b.mu.Lock()
	p := b.reqs[chunk.GetRequestId()]
	b.mu.Unlock()
	if p == nil || p.ChunkCh == nil {
		return
	}
	select {
	case p.ChunkCh <- chunk:
	default:
		// Buffer full — try blocking briefly so we don't drop mid-stream.
		select {
		case p.ChunkCh <- chunk:
		case <-time.After(5 * time.Second):
		}
	}
}

func (b *Broker) remove(id string) {
	b.mu.Lock()
	delete(b.reqs, id)
	b.mu.Unlock()
}

func drainListing(ch chan *pb.DirListing) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func drainChunks(ch chan *pb.FileChunk) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}
