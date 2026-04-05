package httpapi

import (
	"testing"
	"time"
)

func TestTypingHubSubscribeWithSnapshotIncludesExistingPresence(t *testing.T) {
	t.Parallel()

	hub := newTypingHub(4)
	now := time.Unix(1700000000, 0).UTC()
	start := hub.Start("room_a", "agent_a", now, 5*time.Second)

	sub, snapshot := hub.SubscribeWithSnapshot("room_a", now.Add(100*time.Millisecond))
	defer sub.Close()

	if len(snapshot) != 1 {
		t.Fatalf("snapshot len=%d want=1", len(snapshot))
	}
	if got := snapshot[0]; got.ActorID != start.ActorID || got.State != start.State || !got.ExpiresAt.Equal(start.ExpiresAt) {
		t.Fatalf("snapshot=%+v want actor=%q state=%q expires_at=%s", got, start.ActorID, start.State, start.ExpiresAt)
	}

	expectNoTypingEvent(t, sub.Events(), 150*time.Millisecond)
}

func TestTypingHubSubscribeWithSnapshotDeliversSubsequentPresenceLive(t *testing.T) {
	t.Parallel()

	hub := newTypingHub(4)
	now := time.Unix(1700000000, 0).UTC()

	sub, snapshot := hub.SubscribeWithSnapshot("room_a", now)
	defer sub.Close()

	if len(snapshot) != 0 {
		t.Fatalf("snapshot len=%d want=0", len(snapshot))
	}

	start := hub.Start("room_a", "agent_a", now.Add(100*time.Millisecond), 5*time.Second)
	event := expectTypingEvent(t, sub.Events())
	if event.ActorID != start.ActorID || event.State != start.State || !event.ExpiresAt.Equal(start.ExpiresAt) {
		t.Fatalf("event=%+v want actor=%q state=%q expires_at=%s", event, start.ActorID, start.State, start.ExpiresAt)
	}
}

func expectTypingEvent(t *testing.T, ch <-chan roomTypingEvent) roomTypingEvent {
	t.Helper()

	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while waiting for typing event")
		}
		return event
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for typing event")
		return roomTypingEvent{}
	}
}

func expectNoTypingEvent(t *testing.T, ch <-chan roomTypingEvent, d time.Duration) {
	t.Helper()

	select {
	case event, ok := <-ch:
		if !ok {
			return
		}
		t.Fatalf("unexpected typing event state=%q actor=%q", event.State, event.ActorID)
	case <-time.After(d):
	}
}
