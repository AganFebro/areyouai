package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/domain"
	"github.com/febrian/areyouai/internal/repository"
)

type recordRoomContextFetchConflictStore struct {
	repository.Store

	room        repository.Room
	context     repository.RoomContextState
	upsertCalls int
}

func (s *recordRoomContextFetchConflictStore) GetRoom(ctx context.Context, roomID string) (repository.Room, error) {
	if roomID != s.room.ID {
		return repository.Room{}, repository.ErrNotFound
	}
	return s.room, nil
}

func (s *recordRoomContextFetchConflictStore) GetRoomContext(ctx context.Context, roomID string) (repository.RoomContextState, error) {
	if roomID != s.room.ID {
		return repository.RoomContextState{}, repository.ErrNotFound
	}
	return s.context, nil
}

func (s *recordRoomContextFetchConflictStore) UpsertRoomContext(ctx context.Context, in repository.UpsertRoomContextInput) (repository.RoomContextState, error) {
	s.upsertCalls++
	if s.upsertCalls == 1 {
		payload := roomContextPayload{
			RoomID:                      s.room.ID,
			Topic:                       s.room.Topic,
			ConversationMode:            inferConversationMode(s.room.Topic),
			AgentAID:                    s.room.AgentAID,
			AgentBID:                    s.room.AgentBID,
			LastContextFetchTurnByAgent: map[string]int{s.room.AgentBID: s.room.TurnIndex},
			State:                       string(s.room.State),
			TurnIndex:                   s.room.TurnIndex,
			MaxTurns:                    s.room.MaxTurns,
			TTLAt:                       s.room.TTLAt.Format(time.RFC3339),
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return repository.RoomContextState{}, err
		}
		s.context = repository.RoomContextState{
			RoomID:  s.room.ID,
			Context: raw,
			Version: 2,
		}
		return repository.RoomContextState{}, repository.ErrConflict
	}

	s.context = repository.RoomContextState{
		RoomID:  in.RoomID,
		Context: in.Context,
		Version: in.Version,
	}
	return s.context, nil
}

type transcriptRoomContextReadFailStore struct {
	repository.Store

	room     repository.Room
	messages []repository.Message
}

func (s *transcriptRoomContextReadFailStore) GetRoom(ctx context.Context, roomID string) (repository.Room, error) {
	if roomID != s.room.ID {
		return repository.Room{}, repository.ErrNotFound
	}
	return s.room, nil
}

func (s *transcriptRoomContextReadFailStore) ListRoomMessages(ctx context.Context, roomID string) ([]repository.Message, error) {
	if roomID != s.room.ID {
		return nil, repository.ErrNotFound
	}
	out := make([]repository.Message, len(s.messages))
	copy(out, s.messages)
	return out, nil
}

func (s *transcriptRoomContextReadFailStore) GetRoomContext(ctx context.Context, roomID string) (repository.RoomContextState, error) {
	return repository.RoomContextState{}, fmt.Errorf("forced room context read failure")
}

func TestRecordRoomContextFetchRetriesConflictAndPreservesMarkers(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0).UTC()
	initialPayload := roomContextPayload{
		RoomID:                      "room_ctx",
		Topic:                       "conflict merge",
		ConversationMode:            "normal_chat",
		AgentAID:                    "agt_a",
		AgentBID:                    "agt_b",
		LastContextFetchTurnByAgent: map[string]int{},
		State:                       string(domain.RoomStateActive),
		TurnIndex:                   3,
		MaxTurns:                    8,
		TTLAt:                       now.Add(1 * time.Hour).Format(time.RFC3339),
	}
	raw, err := json.Marshal(initialPayload)
	if err != nil {
		t.Fatalf("marshal initial payload: %v", err)
	}

	store := &recordRoomContextFetchConflictStore{
		room: repository.Room{
			ID:        "room_ctx",
			Topic:     "conflict merge",
			AgentAID:  "agt_a",
			AgentBID:  "agt_b",
			State:     domain.RoomStateActive,
			TurnIndex: 3,
			MaxTurns:  8,
			TTLAt:     now.Add(1 * time.Hour),
		},
		context: repository.RoomContextState{
			RoomID:  "room_ctx",
			Context: raw,
			Version: 1,
		},
	}

	svc := New(store, Options{})
	svc.now = func() time.Time { return now }

	if err := svc.RecordRoomContextFetch(context.Background(), "agt_a", "room_ctx", 3); err != nil {
		t.Fatalf("RecordRoomContextFetch() error = %v", err)
	}
	if store.upsertCalls != 2 {
		t.Fatalf("upsert calls=%d want=2", store.upsertCalls)
	}

	var persisted roomContextPayload
	if err := json.Unmarshal(store.context.Context, &persisted); err != nil {
		t.Fatalf("unmarshal persisted payload: %v", err)
	}
	if got := persisted.LastContextFetchTurnByAgent["agt_a"]; got != 3 {
		t.Fatalf("fetch marker agent A=%d want=3", got)
	}
	if got := persisted.LastContextFetchTurnByAgent["agt_b"]; got != 3 {
		t.Fatalf("fetch marker agent B=%d want=3", got)
	}
}

func TestTranscriptIgnoresRoomContextReadFailures(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0).UTC()
	svc := New(&transcriptRoomContextReadFailStore{
		room: repository.Room{
			ID:            "room_transcript",
			Topic:         "transcript fallback",
			AgentAID:      "agt_a",
			AgentBID:      "agt_b",
			State:         domain.RoomStateActive,
			TurnIndex:     1,
			MaxTurns:      4,
			TTLAt:         now.Add(1 * time.Hour),
			HumanCodeHash: hashText("hc_ok"),
		},
		messages: []repository.Message{
			{
				ID:         "msg_1",
				RoomID:     "room_transcript",
				SenderID:   "agt_a",
				SenderName: "agent a",
				Turn:       0,
				Ciphertext: "hello",
				CreatedAt:  now,
			},
		},
	}, Options{})
	svc.now = func() time.Time { return now }

	out, err := svc.Transcript(context.Background(), "room_transcript", "hc_ok")
	if err != nil {
		t.Fatalf("Transcript() error = %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages len=%d want=1", len(out.Messages))
	}
	if len(out.LastContextFetchByAgent) != 0 {
		t.Fatalf("LastContextFetchByAgent=%v want empty", out.LastContextFetchByAgent)
	}
}
