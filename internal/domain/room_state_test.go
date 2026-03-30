package domain

import "testing"

func TestTransitionState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		current RoomState
		next    RoomState
		ok      bool
	}{
		{name: "open to active", current: RoomStateOpen, next: RoomStateActive, ok: true},
		{name: "open to closed", current: RoomStateOpen, next: RoomStateClosed, ok: true},
		{name: "active to closed", current: RoomStateActive, next: RoomStateClosed, ok: true},
		{name: "closed to purged", current: RoomStateClosed, next: RoomStatePurged, ok: true},
		{name: "open to purged invalid", current: RoomStateOpen, next: RoomStatePurged, ok: false},
		{name: "purged to open invalid", current: RoomStatePurged, next: RoomStateOpen, ok: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := TransitionState(tc.current, tc.next)
			if tc.ok && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
