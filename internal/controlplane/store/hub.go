package store

import "sync"

// Hub fans out per-machine change notifications to WatchMachine subscribers
// and a fleet-wide SubscribeAll used by cluster controllers.
// Buffered channels coalesce bursts so a slow UI never
// blocks the agent stream or write path.
type Hub struct {
	mu      sync.Mutex
	subs    map[string]map[chan struct{}]struct{} // machineID -> set of channels
	allSubs map[chan string]struct{}
}

// NewHub returns an empty fan-out hub.
func NewHub() *Hub {
	return &Hub{
		subs:    map[string]map[chan struct{}]struct{}{},
		allSubs: map[chan string]struct{}{},
	}
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

// SubscribeAll signals every machine change, delivering the machine ID.
// Buffered and coalescing like Subscribe: a slow subscriber drops rather
// than blocking Notify. Controllers are level-triggered, so a dropped ID
// is recovered on the next signal or tick.
func (h *Hub) SubscribeAll() (<-chan string, func()) {
	ch := make(chan string, 1)
	h.mu.Lock()
	h.allSubs[ch] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		delete(h.allSubs, ch)
		h.mu.Unlock()
	}
	return ch, cancel
}

// Notify signals all subscribers of machineID (non-blocking, coalesced),
// including SubscribeAll watchers.
func (h *Hub) Notify(machineID string) {
	h.mu.Lock()
	subs := make([]chan struct{}, 0, len(h.subs[machineID]))
	for ch := range h.subs[machineID] {
		subs = append(subs, ch)
	}
	alls := make([]chan string, 0, len(h.allSubs))
	for ch := range h.allSubs {
		alls = append(alls, ch)
	}
	h.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	for _, ch := range alls {
		select {
		case ch <- machineID:
		default:
		}
	}
}
