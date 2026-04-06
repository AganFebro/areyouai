package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

type recentRoomMessagesBoundedStore struct {
	repository.Store

	room      repository.Room
	messages  []repository.Message
	requested int
}

func (s *recentRoomMessagesBoundedStore) GetRoom(ctx context.Context, roomID string) (repository.Room, error) {
	if roomID != s.room.ID {
		return repository.Room{}, repository.ErrNotFound
	}
	return s.room, nil
}

func (s *recentRoomMessagesBoundedStore) GetRoomContext(ctx context.Context, roomID string) (repository.RoomContextState, error) {
	return repository.RoomContextState{}, repository.ErrNotFound
}

func (s *recentRoomMessagesBoundedStore) ListRecentRoomMessages(ctx context.Context, roomID string, limit int) ([]repository.Message, error) {
	if roomID != s.room.ID {
		return nil, repository.ErrNotFound
	}
	s.requested = limit
	if limit > len(s.messages) {
		limit = len(s.messages)
	}
	out := make([]repository.Message, limit)
	copy(out, s.messages[len(s.messages)-limit:])
	return out, nil
}

func (s *recentRoomMessagesBoundedStore) ListRoomMessages(ctx context.Context, roomID string) ([]repository.Message, error) {
	panic("buildBundleForRoom should use ListRecentRoomMessages")
}

func (s *recentRoomMessagesBoundedStore) AppendAuditEvent(ctx context.Context, in repository.AppendAuditEventInput) error {
	return nil
}

type reconcileRoomConcurrencyStore struct {
	repository.Store

	rooms       map[string]repository.Room
	blockRoomID string
	started     chan struct{}
	release     chan struct{}
}

func (s *reconcileRoomConcurrencyStore) GetRoom(ctx context.Context, roomID string) (repository.Room, error) {
	room, ok := s.rooms[roomID]
	if !ok {
		return repository.Room{}, repository.ErrNotFound
	}
	return room, nil
}

func (s *reconcileRoomConcurrencyStore) CountActiveViewers(ctx context.Context, roomID string, activeSince time.Time) (int, error) {
	if roomID == s.blockRoomID {
		select {
		case <-s.started:
		default:
			close(s.started)
		}
		<-s.release
	}
	return 0, nil
}

func (s *reconcileRoomConcurrencyStore) ListRoomMessages(ctx context.Context, roomID string) ([]repository.Message, error) {
	return nil, nil
}

func (s *reconcileRoomConcurrencyStore) WithTx(ctx context.Context, fn func(ctx context.Context, tx repository.TxStore) error) error {
	return fn(ctx, s)
}

func (s *reconcileRoomConcurrencyStore) UpdateRoom(ctx context.Context, in repository.UpdateRoomInput) (repository.Room, error) {
	room, ok := s.rooms[in.ID]
	if !ok {
		return repository.Room{}, repository.ErrNotFound
	}
	if in.State != nil {
		room.State = *in.State
	}
	if in.ClosedAt != nil {
		room.ClosedAt = in.ClosedAt
	}
	if in.PurgedAt != nil {
		room.PurgedAt = in.PurgedAt
	}
	s.rooms[in.ID] = room
	return room, nil
}

func (s *reconcileRoomConcurrencyStore) AppendRoomEvent(ctx context.Context, in repository.AppendRoomEventInput) (repository.RoomEvent, error) {
	return repository.RoomEvent{}, nil
}

func (s *reconcileRoomConcurrencyStore) PurgeRoomContent(ctx context.Context, roomID string, purgedAt time.Time) error {
	room, ok := s.rooms[roomID]
	if !ok {
		return repository.ErrNotFound
	}
	room.PurgedAt = &purgedAt
	s.rooms[roomID] = room
	return nil
}

func (s *reconcileRoomConcurrencyStore) CreateRoomScopedToken(ctx context.Context, in repository.CreateRoomScopedTokenInput) (repository.RoomScopedToken, error) {
	return repository.RoomScopedToken{}, nil
}

func (s *reconcileRoomConcurrencyStore) RevokeRoomScopedTokens(ctx context.Context, roomID, agentID string, revokedAt time.Time) error {
	return nil
}

func (s *reconcileRoomConcurrencyStore) CreateAgentStreamDelivery(ctx context.Context, in repository.CreateAgentStreamDeliveryInput) (repository.AgentStreamDelivery, error) {
	return repository.AgentStreamDelivery{}, nil
}

func (s *reconcileRoomConcurrencyStore) ListPendingAgentStreamDeliveries(ctx context.Context, agentID string, afterSeq int64, now time.Time, limit int) ([]repository.AgentStreamDelivery, error) {
	return nil, nil
}

func (s *reconcileRoomConcurrencyStore) AckAgentStreamDelivery(ctx context.Context, agentID, deliveryID string, ackedAt time.Time) error {
	return nil
}

func (s *reconcileRoomConcurrencyStore) CreateListing(ctx context.Context, in repository.CreateListingInput) (repository.Listing, error) {
	return repository.Listing{}, nil
}

func (s *reconcileRoomConcurrencyStore) GetListing(ctx context.Context, listingID string) (repository.Listing, error) {
	return repository.Listing{}, repository.ErrNotFound
}

func (s *reconcileRoomConcurrencyStore) MarkListingConnected(ctx context.Context, listingID string) error {
	return nil
}
func (s *reconcileRoomConcurrencyStore) SearchListings(ctx context.Context, query string) ([]repository.Listing, error) {
	return nil, nil
}
func (s *reconcileRoomConcurrencyStore) CreateRoom(ctx context.Context, in repository.CreateRoomInput) (repository.Room, error) {
	return repository.Room{}, nil
}
func (s *reconcileRoomConcurrencyStore) AppendMessage(ctx context.Context, in repository.AppendMessageInput) (repository.Message, error) {
	return repository.Message{}, nil
}
func (s *reconcileRoomConcurrencyStore) GetRoomContext(ctx context.Context, roomID string) (repository.RoomContextState, error) {
	return repository.RoomContextState{}, repository.ErrNotFound
}
func (s *reconcileRoomConcurrencyStore) UpsertRoomContext(ctx context.Context, in repository.UpsertRoomContextInput) (repository.RoomContextState, error) {
	return repository.RoomContextState{}, nil
}
func (s *reconcileRoomConcurrencyStore) UpsertViewer(ctx context.Context, in repository.UpsertViewerInput) (repository.Viewer, error) {
	return repository.Viewer{}, nil
}
func (s *reconcileRoomConcurrencyStore) GetViewer(ctx context.Context, viewerToken string) (repository.Viewer, error) {
	return repository.Viewer{}, repository.ErrNotFound
}
func (s *reconcileRoomConcurrencyStore) AppendAuditEvent(ctx context.Context, in repository.AppendAuditEventInput) error {
	return nil
}
func (s *reconcileRoomConcurrencyStore) AppendAPIRequestLog(ctx context.Context, in repository.AppendAPIRequestLogInput) error {
	return nil
}
func (s *reconcileRoomConcurrencyStore) GetRoomEvent(ctx context.Context, eventID int64) (repository.RoomEvent, error) {
	return repository.RoomEvent{}, repository.ErrNotFound
}
func (s *reconcileRoomConcurrencyStore) ListRoomEvents(ctx context.Context, in repository.ListRoomEventsInput) ([]repository.RoomEvent, error) {
	return nil, nil
}
func (s *reconcileRoomConcurrencyStore) GetAgentStreamDelivery(ctx context.Context, agentID, deliveryID string) (repository.AgentStreamDelivery, error) {
	return repository.AgentStreamDelivery{}, repository.ErrNotFound
}
func (s *reconcileRoomConcurrencyStore) ListRecoverableRoomsForAgent(ctx context.Context, agentID string, since time.Time) ([]repository.Room, error) {
	return nil, nil
}
func (s *reconcileRoomConcurrencyStore) CreateAgentWebhookEndpoint(ctx context.Context, in repository.CreateAgentWebhookEndpointInput) (repository.AgentWebhookEndpoint, error) {
	return repository.AgentWebhookEndpoint{}, nil
}
func (s *reconcileRoomConcurrencyStore) ListAgentWebhookEndpoints(ctx context.Context, agentID string) ([]repository.AgentWebhookEndpoint, error) {
	return nil, nil
}
func (s *reconcileRoomConcurrencyStore) DeleteAgentWebhookEndpoint(ctx context.Context, agentID, endpointID string) error {
	return nil
}
func (s *reconcileRoomConcurrencyStore) CreateWebhookOutbox(ctx context.Context, in repository.CreateWebhookOutboxInput) (repository.WebhookOutboxItem, error) {
	return repository.WebhookOutboxItem{}, nil
}
func (s *reconcileRoomConcurrencyStore) ClaimPendingWebhookDeliveries(ctx context.Context, now, reclaimBefore time.Time, limit int) ([]repository.ClaimedWebhookDelivery, error) {
	return nil, nil
}
func (s *reconcileRoomConcurrencyStore) MarkWebhookOutboxDelivered(ctx context.Context, id int64) error {
	return nil
}
func (s *reconcileRoomConcurrencyStore) MarkWebhookOutboxPendingRetry(ctx context.Context, id int64, nextAttemptAt time.Time, lastError string) error {
	return nil
}
func (s *reconcileRoomConcurrencyStore) MarkWebhookOutboxDeadLetter(ctx context.Context, id int64, lastError string) error {
	return nil
}
func (s *reconcileRoomConcurrencyStore) FindRoomScopedTokenByHash(ctx context.Context, tokenHash string) (repository.RoomScopedToken, error) {
	return repository.RoomScopedToken{}, repository.ErrNotFound
}
func (s *reconcileRoomConcurrencyStore) TouchRoomScopedToken(ctx context.Context, tokenHash string, lastUsedAt, expiresAt time.Time) error {
	return nil
}
func (s *reconcileRoomConcurrencyStore) GetAdminOverview(ctx context.Context, now time.Time) (repository.AdminOverview, error) {
	return repository.AdminOverview{}, nil
}
func (s *reconcileRoomConcurrencyStore) ListAdminRooms(ctx context.Context, limit int) ([]repository.AdminRoom, error) {
	return nil, nil
}
func (s *reconcileRoomConcurrencyStore) ListAuditEvents(ctx context.Context, limit int) ([]repository.AuditEvent, error) {
	return nil, nil
}

func TestReconcileRoomUsesPerRoomLock(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0).UTC()
	release := make(chan struct{})
	started := make(chan struct{})
	store := &reconcileRoomConcurrencyStore{
		rooms: map[string]repository.Room{
			"room_a": {ID: "room_a", State: domain.RoomStateClosed, ClosedAt: &now, TTLAt: now.Add(1 * time.Hour)},
			"room_b": {ID: "room_b", State: domain.RoomStateClosed, ClosedAt: &now, TTLAt: now.Add(1 * time.Hour)},
		},
		blockRoomID: "room_a",
		started:     started,
		release:     release,
	}
	svc := New(store, Options{})
	svc.now = func() time.Time { return now }

	roomA := store.rooms["room_a"]
	roomB := store.rooms["room_b"]
	errChA := make(chan error, 1)
	errChB := make(chan error, 1)

	go func() {
		_, err := svc.reconcileRoom(context.Background(), roomA)
		errChA <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("room A did not enter reconcile in time")
	}

	doneB := make(chan struct{})
	go func() {
		_, err := svc.reconcileRoom(context.Background(), roomB)
		errChB <- err
		close(doneB)
	}()

	select {
	case <-doneB:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("room B reconcile was blocked behind room A")
	}

	close(release)
	if err := <-errChA; err != nil {
		t.Fatalf("room A reconcile err=%v", err)
	}
	if err := <-errChB; err != nil {
		t.Fatalf("room B reconcile err=%v", err)
	}
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

func (s *transcriptRoomContextReadFailStore) AppendAuditEvent(ctx context.Context, in repository.AppendAuditEventInput) error {
	return nil
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

func TestBuildBundleForRoomIgnoresRoomContextReadFailures(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0).UTC()
	svc := New(&transcriptRoomContextReadFailStore{
		room: repository.Room{
			ID:        "room_bundle",
			Topic:     "bundle fallback",
			AgentAID:  "agt_a",
			AgentBID:  "agt_b",
			State:     domain.RoomStateActive,
			TurnIndex: 1,
			MaxTurns:  4,
			TTLAt:     now.Add(1 * time.Hour),
		},
		messages: []repository.Message{
			{
				ID:         "msg_1",
				RoomID:     "room_bundle",
				SenderID:   "agt_a",
				SenderName: "agent a",
				Turn:       0,
				Ciphertext: "hello",
				CreatedAt:  now,
			},
		},
	}, Options{})
	svc.now = func() time.Time { return now }

	bundle, count, err := svc.buildBundleForRoom(context.Background(), repository.Room{
		ID:        "room_bundle",
		Topic:     "bundle fallback",
		AgentAID:  "agt_a",
		AgentBID:  "agt_b",
		State:     domain.RoomStateActive,
		TurnIndex: 1,
		MaxTurns:  4,
		TTLAt:     now.Add(1 * time.Hour),
	}, "agt_a")
	if err != nil {
		t.Fatalf("buildBundleForRoom() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("recent count=%d want=1", count)
	}
	if !strings.Contains(bundle.Prompt, "room_id=room_bundle") {
		t.Fatalf("bundle prompt missing room context: %s", bundle.Prompt)
	}
}

func TestBuildBundleForRoomUsesBoundedRecentMessages(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0).UTC()
	messages := make([]repository.Message, 10)
	for i := range messages {
		messages[i] = repository.Message{
			ID:         fmt.Sprintf("msg_%d", i),
			RoomID:     "room_recent",
			SenderID:   "agt_a",
			SenderName: "agent a",
			Turn:       i,
			Ciphertext: fmt.Sprintf("message_%d", i),
			CreatedAt:  now.Add(time.Duration(i) * time.Minute),
		}
	}
	store := &recentRoomMessagesBoundedStore{
		room: repository.Room{
			ID:        "room_recent",
			Topic:     "bounded recent messages",
			AgentAID:  "agt_a",
			AgentBID:  "agt_b",
			State:     domain.RoomStateActive,
			TurnIndex: 10,
			MaxTurns:  20,
			TTLAt:     now.Add(1 * time.Hour),
		},
		messages: messages,
	}
	svc := New(store, Options{})
	svc.now = func() time.Time { return now }

	bundle, count, err := svc.buildBundleForRoom(context.Background(), store.room, "agt_a")
	if err != nil {
		t.Fatalf("buildBundleForRoom() error = %v", err)
	}
	if store.requested != maxContextRecentMessages {
		t.Fatalf("requested limit=%d want=%d", store.requested, maxContextRecentMessages)
	}
	if count != maxContextRecentMessages {
		t.Fatalf("recent count=%d want=%d", count, maxContextRecentMessages)
	}
	if strings.Contains(bundle.Prompt, "message_0") || strings.Contains(bundle.Prompt, "message_1") || strings.Contains(bundle.Prompt, "message_2") || strings.Contains(bundle.Prompt, "message_3") {
		t.Fatalf("bundle prompt should only use recent messages: %s", bundle.Prompt)
	}
}
