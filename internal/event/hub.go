package event

import "sync"

// subscriberBuffer is how many events a subscriber may lag behind before it is
// dropped. A slow client must never slow down a run.
const subscriberBuffer = 512

// globalSubscriberBuffer is larger because one stream carries every session.
const globalSubscriberBuffer = 2048

// Hub fans events out to live subscribers, grouped by session. A global
// subscriber receives every session event tagged with its session ID.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
	all  map[*GlobalSubscription]struct{}
}

func NewHub() *Hub {
	return &Hub{
		subs: map[string]map[*Subscription]struct{}{},
		all:  map[*GlobalSubscription]struct{}{},
	}
}

type Subscription struct {
	C    chan Event
	once sync.Once
	hub  *Hub
	id   string
}

// SessionEvent is one event plus the session it belongs to.
type SessionEvent struct {
	SessionID string
	Event     Event
}

// GlobalSubscription receives every session event.
type GlobalSubscription struct {
	C    chan SessionEvent
	once sync.Once
	hub  *Hub
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

// SubscribeAll starts receiving live events for every session.
func (h *Hub) SubscribeAll() *GlobalSubscription {
	s := &GlobalSubscription{C: make(chan SessionEvent, globalSubscriberBuffer), hub: h}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.all[s] = struct{}{}
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

// Close stops delivery and closes the channel. It is safe to call twice.
func (s *GlobalSubscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		delete(s.hub.all, s)
		close(s.C)
	})
}

func (s *Subscription) Events() <-chan Event { return s.C }

func (s *GlobalSubscription) Events() <-chan SessionEvent { return s.C }

// Publish delivers ev to every subscriber of the session and to global
// subscribers. A subscriber that is full is dropped instead of blocking the
// publisher.
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
	for s := range h.all {
		select {
		case s.C <- SessionEvent{SessionID: sessionID, Event: ev}:
		default:
			delete(h.all, s)
			s.once.Do(func() { close(s.C) })
		}
	}
}
