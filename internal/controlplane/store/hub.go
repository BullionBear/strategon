package store

import "sync"

// Hub fans out per-machine change notifications to WatchMachine subscribers
// (FRONTEND.md §2.3). Buffered channels coalesce bursts so a slow UI never
// blocks the agent stream or write path.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{} // machineID -> set of channels
}

// NewHub returns an empty fan-out hub.
func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan struct{}]struct{}{}}
}

// Subscribe registers a buffered notifier for machineID. The returned channel
// receives a signal whenever that machine's state changes. Call the returned
// cancel func to unsubscribe.
func (h *Hub) Subscribe(machineID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[machineID] == nil {
		h.subs[machineID] = map[chan struct{}]struct{}{}
	}
	h.subs[machineID][ch] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		delete(h.subs[machineID], ch)
		if len(h.subs[machineID]) == 0 {
			delete(h.subs, machineID)
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

// Notify signals all subscribers of machineID (non-blocking, coalesced).
func (h *Hub) Notify(machineID string) {
	h.mu.Lock()
	subs := h.subs[machineID]
	h.mu.Unlock()
	for ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
