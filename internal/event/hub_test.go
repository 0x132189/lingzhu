package event

import "testing"

func TestGlobalSubscriptionReceivesSessionEvents(t *testing.T) {
	hub := NewHub()
	sub := hub.SubscribeAll()
	defer sub.Close()

	hub.Publish("s1", User("hello"))
	select {
	case item := <-sub.Events():
		if item.SessionID != "s1" || item.Event.Type != UserMessage {
			t.Fatalf("unexpected global event: %+v", item)
		}
	default:
		t.Fatal("global subscriber did not receive the event")
	}
}
