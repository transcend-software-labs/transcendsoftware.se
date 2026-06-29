// Package stream is a tiny in-process pub/sub for live build progress.
//
// The orchestrator publishes log events per project; the web layer's SSE
// endpoint subscribes and relays them to the browser (HTMX SSE extension).
// A small per-project history lets a late or reconnecting subscriber catch up.
package stream

import "sync"

// Event is one progress message for a project.
type Event struct {
	Type string // "log" (and room for more later, e.g. "status")
	Data string
}

// Broker fans out events to per-project subscribers and retains recent history.
type Broker struct {
	mu       sync.Mutex
	subs     map[string]map[chan Event]struct{}
	history  map[string][]Event
	histSize int
}

// NewBroker returns a broker retaining up to histSize recent events per project.
func NewBroker(histSize int) *Broker {
	if histSize <= 0 {
		histSize = 500
	}
	return &Broker{
		subs:     make(map[string]map[chan Event]struct{}),
		history:  make(map[string][]Event),
		histSize: histSize,
	}
}

// Reset clears a project's history (call at the start of a new build pass).
func (b *Broker) Reset(projectID string) {
	b.mu.Lock()
	delete(b.history, projectID)
	b.mu.Unlock()
}

// Publish appends to history and delivers to live subscribers (non-blocking).
func (b *Broker) Publish(projectID string, e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	h := append(b.history[projectID], e)
	if len(h) > b.histSize {
		h = h[len(h)-b.histSize:]
	}
	b.history[projectID] = h

	for ch := range b.subs[projectID] {
		select {
		case ch <- e:
		default: // drop for a slow subscriber rather than block the build
		}
	}
}

// Subscribe returns the current history plus a live channel and a cancel func.
func (b *Broker) Subscribe(projectID string) (history []Event, ch <-chan Event, cancel func()) {
	c := make(chan Event, 64)
	b.mu.Lock()
	hist := make([]Event, len(b.history[projectID]))
	copy(hist, b.history[projectID])
	if b.subs[projectID] == nil {
		b.subs[projectID] = make(map[chan Event]struct{})
	}
	b.subs[projectID][c] = struct{}{}
	b.mu.Unlock()

	cancel = func() {
		b.mu.Lock()
		if m := b.subs[projectID]; m != nil {
			delete(m, c)
			if len(m) == 0 {
				delete(b.subs, projectID)
			}
		}
		b.mu.Unlock()
		close(c)
	}
	return hist, c, cancel
}
