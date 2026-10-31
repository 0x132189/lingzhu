package event

import "sync"

// subscriberBuffer is how many events a subscriber may lag behind before it is
// dropped. A slow client must never slow down a run.
const subscriberBuffer = 512

// Hub fans events out to live subscribers, grouped by session.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[string]map[*Subscription]struct{}{}}
}

type Subscription struct {
	C    chan Event
	once sync.Once
	hub  *Hub
	id   string
}

// Subscribe starts receiving live events for one session.
func (h *Hub) Subscribe(sessionID string) *Subscription {
	s := &Subscription{C: make(chan Event, subscriberBuffer), hub: h, id: sessionID}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = map[*Subscription]struct{}{}
	}
	h.subs[sessionID][s] = struct{}{}
	return s
}

// Close stops delivery and closes the channel. It is safe to call twice.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		if subs := s.hub.subs[s.id]; subs != nil {
			delete(subs, s)
			if len(subs) == 0 {
				delete(s.hub.subs, s.id)
			}
		}
		close(s.C)
	})
}

func (s *Subscription) Events() <-chan Event { return s.C }

// Publish delivers ev to every subscriber of the session. A subscriber that is
// full is dropped instead of blocking the publisher.
func (h *Hub) Publish(sessionID string, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[sessionID] {
		select {
		case s.C <- ev:
		default:
			delete(h.subs[sessionID], s)
			s.once.Do(func() { close(s.C) })
		}
	}
}
