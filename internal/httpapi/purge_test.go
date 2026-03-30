package httpapi

import (
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/domain"
)

func TestPurgeWaitsForNoActiveViewerAfterGrace(t *testing.T) {
	t.Parallel()

	a := newApp(options{})
	base := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	now := base
	a.now = func() time.Time { return now }

	closedAt := now.Add(-3 * time.Minute)
	a.rooms["room_1"] = room{
		ID:            "room_1",
		State:         domain.RoomStateClosed,
		ClosedAt:      &closedAt,
		Viewers:       map[string]viewerSession{"v1": {Token: "v1", LastHeartbeatAt: now}},
		Messages:      []message{{ID: "m1", Ciphertext: "cipher-1"}},
		HumanCodeHash: hashText("hc_test"),
	}

	a.purgeSweep()
	if got := a.rooms["room_1"].State; got != domain.RoomStateClosed {
		t.Fatalf("expected CLOSED while viewer active, got %s", got)
	}

	now = now.Add(1 * time.Minute)
	a.purgeSweep()

	rm := a.rooms["room_1"]
	if rm.State != domain.RoomStatePurged {
		t.Fatalf("expected PURGED after inactive viewer + grace, got %s", rm.State)
	}
	if len(rm.Messages) != 0 {
		t.Fatalf("expected messages hard-deleted on purge, got %d", len(rm.Messages))
	}
	if len(a.auditEvents) == 0 {
		t.Fatal("expected purge audit event")
	}
}

func TestPurgeByMaxRetentionEvenWithActiveViewer(t *testing.T) {
	t.Parallel()

	a := newApp(options{})
	base := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return base }

	closedAt := base.Add(-25 * time.Hour)
	a.rooms["room_2"] = room{
		ID:       "room_2",
		State:    domain.RoomStateClosed,
		ClosedAt: &closedAt,
		Viewers: map[string]viewerSession{
			"v1": {Token: "v1", LastHeartbeatAt: base},
		},
		Messages:      []message{{ID: "m1", Ciphertext: "cipher-1"}},
		HumanCodeHash: hashText("hc_test"),
	}

	a.purgeSweep()
	if got := a.rooms["room_2"].State; got != domain.RoomStatePurged {
		t.Fatalf("expected PURGED by retention cap, got %s", got)
	}
}
