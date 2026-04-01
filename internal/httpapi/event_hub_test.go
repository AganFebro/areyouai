package httpapi

import (
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/repository"
)

func TestRoomEventHubFanoutByRoom(t *testing.T) {
	t.Parallel()

	hub := newRoomEventHub(4)
	subA1 := hub.Subscribe("room_a")
	defer subA1.Close()
	subA2 := hub.Subscribe("room_a")
	defer subA2.Close()
	subB := hub.Subscribe("room_b")
	defer subB.Close()

	hub.Publish(repository.RoomEvent{ID: 1, RoomID: "room_a", EventType: "message.created"})

	expectEventID(t, subA1.Events(), 1)
	expectEventID(t, subA2.Events(), 1)
	expectNoEvent(t, subB.Events(), 150*time.Millisecond)

	hub.Publish(repository.RoomEvent{ID: 2, RoomID: "room_b", EventType: "room.closed"})

	expectEventID(t, subB.Events(), 2)
	expectNoEvent(t, subA1.Events(), 150*time.Millisecond)
	expectNoEvent(t, subA2.Events(), 150*time.Millisecond)
}

func TestRoomEventHubDropsSlowSubscriber(t *testing.T) {
	t.Parallel()

	hub := newRoomEventHub(1)
	slow := hub.Subscribe("room_drop")
	defer slow.Close()
	fast := hub.Subscribe("room_drop")
	defer fast.Close()

	hub.Publish(repository.RoomEvent{ID: 10, RoomID: "room_drop", EventType: "message.created"})
	expectEventID(t, fast.Events(), 10)

	// Slow subscriber buffer remains full (ID 10 unread), next publish drops it.
	hub.Publish(repository.RoomEvent{ID: 11, RoomID: "room_drop", EventType: "message.created"})
	expectEventID(t, fast.Events(), 11)

	// Slow subscriber can drain buffered item, then channel must close.
	expectEventID(t, slow.Events(), 10)
	select {
	case _, ok := <-slow.Events():
		if ok {
			t.Fatal("expected slow subscriber channel closed after drop")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for dropped subscriber close")
	}

	// Fast subscriber must continue receiving new events.
	hub.Publish(repository.RoomEvent{ID: 12, RoomID: "room_drop", EventType: "message.created"})
	expectEventID(t, fast.Events(), 12)
}

func expectEventID(t *testing.T, ch <-chan repository.RoomEvent, wantID int64) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed while waiting for event id=%d", wantID)
		}
		if ev.ID != wantID {
			t.Fatalf("event id=%d want=%d", ev.ID, wantID)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for event id=%d", wantID)
	}
}

func expectNoEvent(t *testing.T, ch <-chan repository.RoomEvent, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			return
		}
		t.Fatalf("unexpected event id=%d type=%q", ev.ID, ev.EventType)
	case <-time.After(d):
	}
}
