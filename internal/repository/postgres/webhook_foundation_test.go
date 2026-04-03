package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/repository"
)

func TestAgentWebhookEndpointsCRUD(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	seedAgentsForEvents(t, ctx, store)

	created, err := store.CreateAgentWebhookEndpoint(ctx, repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_1",
		AgentID:          "agt_a",
		URL:              "https://example.com/hooks/agent",
		SecretCiphertext: "enc-secret",
		KeyID:            "key-1",
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if created.ID != "wh_ep_1" {
		t.Fatalf("endpoint id=%s want=wh_ep_1", created.ID)
	}

	items, err := store.ListAgentWebhookEndpoints(ctx, "agt_a")
	if err != nil {
		t.Fatalf("list endpoints: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("endpoints len=%d want=1", len(items))
	}
	if items[0].URL != created.URL {
		t.Fatalf("endpoint url=%s want=%s", items[0].URL, created.URL)
	}

	if err := store.DeleteAgentWebhookEndpoint(ctx, "agt_a", created.ID); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}
	items, err = store.ListAgentWebhookEndpoints(ctx, "agt_a")
	if err != nil {
		t.Fatalf("list endpoints after delete: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("endpoints len after delete=%d want=0", len(items))
	}
}

func TestDeleteWebhookEndpointRemovesOutboxRows(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)

	endpoint, err := store.CreateAgentWebhookEndpoint(ctx, repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_delete_busy",
		AgentID:          "agt_b",
		URL:              "https://example.com/hooks/delete-busy",
		SecretCiphertext: "enc-delete-busy",
		KeyID:            "key-delete-busy",
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	ev, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:    roomID,
		EventType: "message.created",
	})
	if err != nil {
		t.Fatalf("append room event: %v", err)
	}

	if _, err := store.CreateWebhookOutbox(ctx, repository.CreateWebhookOutboxInput{
		RoomID:        roomID,
		RoomEventID:   ev.ID,
		TargetAgentID: "agt_b",
		EndpointID:    endpoint.ID,
		EventType:     "message.created",
		Payload:       json.RawMessage(`{"delivery_id":"whd_delete_busy_1"}`),
	}); err != nil {
		t.Fatalf("create outbox: %v", err)
	}

	if err := store.DeleteAgentWebhookEndpoint(ctx, "agt_b", endpoint.ID); err != nil {
		t.Fatalf("delete endpoint with outbox rows: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM webhook_outbox WHERE endpoint_id = $1`, endpoint.ID).Scan(&count); err != nil {
		t.Fatalf("count outbox rows after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("outbox rows after endpoint delete=%d want=0", count)
	}
}

func TestWebhookOutboxAndRoomScopedTokensPersist(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)

	endpoint, err := store.CreateAgentWebhookEndpoint(ctx, repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_2",
		AgentID:          "agt_b",
		URL:              "https://example.com/hooks/agent-b",
		SecretCiphertext: "enc-secret-b",
		KeyID:            "key-2",
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	ev, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:    roomID,
		EventType: "room.joined",
	})
	if err != nil {
		t.Fatalf("append room event: %v", err)
	}

	payload := json.RawMessage(`{"type":"room.joined","room_id":"room_events_test"}`)
	outbox, err := store.CreateWebhookOutbox(ctx, repository.CreateWebhookOutboxInput{
		RoomID:        roomID,
		RoomEventID:   ev.ID,
		TargetAgentID: "agt_b",
		EndpointID:    endpoint.ID,
		EventType:     "room.joined",
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("create webhook outbox: %v", err)
	}
	if outbox.RoomEventID != ev.ID {
		t.Fatalf("outbox room_event_id=%d want=%d", outbox.RoomEventID, ev.ID)
	}
	if string(outbox.Payload) != string(payload) {
		t.Fatalf("outbox payload=%s want=%s", string(outbox.Payload), string(payload))
	}

	token, err := store.CreateRoomScopedToken(ctx, repository.CreateRoomScopedTokenInput{
		ID:        "rst_1",
		RoomID:    roomID,
		AgentID:   "agt_b",
		TokenHash: "hash-token-1",
		Scope:     "room:read_write",
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create room scoped token: %v", err)
	}
	found, err := store.FindRoomScopedTokenByHash(ctx, token.TokenHash)
	if err != nil {
		t.Fatalf("find room scoped token: %v", err)
	}
	if found.ID != token.ID {
		t.Fatalf("found token id=%s want=%s", found.ID, token.ID)
	}
	if found.RevokedAt != nil {
		t.Fatalf("token revoked_at=%v want=nil before revoke", found.RevokedAt)
	}

	revokedAt := time.Now().UTC()
	if err := store.RevokeRoomScopedTokens(ctx, roomID, "agt_b", revokedAt); err != nil {
		t.Fatalf("revoke room scoped token: %v", err)
	}
	found, err = store.FindRoomScopedTokenByHash(ctx, token.TokenHash)
	if err != nil {
		t.Fatalf("find room scoped token after revoke: %v", err)
	}
	if found.RevokedAt == nil {
		t.Fatal("expected revoked_at to be set")
	}
}

func TestAgentStreamDeliveriesAndRecoverableRooms(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)

	now := time.Now().UTC()
	payload := json.RawMessage(`{"type":"room.turn_ready","room_id":"room_events_test","next_turn":0}`)
	delivery, err := store.CreateAgentStreamDelivery(ctx, repository.CreateAgentStreamDeliveryInput{
		DeliveryID: "dly_store_1",
		AgentID:    "agt_a",
		RoomID:     roomID,
		Type:       "room.turn_ready",
		Reason:     "room_activated",
		Payload:    payload,
		ExpiresAt:  now.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create agent stream delivery: %v", err)
	}
	if delivery.Seq == 0 {
		t.Fatalf("delivery seq=%d want>0", delivery.Seq)
	}
	if string(delivery.Payload) != string(payload) {
		t.Fatalf("delivery payload=%s want=%s", string(delivery.Payload), string(payload))
	}

	rooms, err := store.ListRecoverableRoomsForAgent(ctx, "agt_a", now.Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("list recoverable rooms: %v", err)
	}
	if len(rooms) != 1 {
		t.Fatalf("recoverable rooms len=%d want=1", len(rooms))
	}
	if rooms[0].ID != roomID {
		t.Fatalf("recoverable room id=%s want=%s", rooms[0].ID, roomID)
	}
}

func TestWebhookOutboxClaimLifecycle(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)

	endpoint, err := store.CreateAgentWebhookEndpoint(ctx, repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_claim",
		AgentID:          "agt_b",
		URL:              "https://example.com/hooks/claim",
		SecretCiphertext: "enc-claim",
		KeyID:            "key-claim",
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	ev1, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:    roomID,
		EventType: "room.joined",
	})
	if err != nil {
		t.Fatalf("append room event 1: %v", err)
	}
	ev2, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
		RoomID:    roomID,
		EventType: "message.created",
	})
	if err != nil {
		t.Fatalf("append room event 2: %v", err)
	}

	now := time.Now().UTC()
	if _, err := store.CreateWebhookOutbox(ctx, repository.CreateWebhookOutboxInput{
		RoomID:        roomID,
		RoomEventID:   ev1.ID,
		TargetAgentID: "agt_b",
		EndpointID:    endpoint.ID,
		EventType:     "room.joined",
		Payload:       json.RawMessage(`{"delivery_id":"whd_claim_1"}`),
	}); err != nil {
		t.Fatalf("create outbox 1: %v", err)
	}
	if _, err := store.CreateWebhookOutbox(ctx, repository.CreateWebhookOutboxInput{
		RoomID:        roomID,
		RoomEventID:   ev2.ID,
		TargetAgentID: "agt_b",
		EndpointID:    endpoint.ID,
		EventType:     "message.created",
		Payload:       json.RawMessage(`{"delivery_id":"whd_claim_2"}`),
	}); err != nil {
		t.Fatalf("create outbox 2: %v", err)
	}

	claimed, err := store.ClaimPendingWebhookDeliveries(ctx, now, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("claim deliveries: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed len=%d want=1", len(claimed))
	}
	if claimed[0].EventType != "room.joined" {
		t.Fatalf("claimed event_type=%s want=room.joined", claimed[0].EventType)
	}
	if claimed[0].AttemptCount != 1 {
		t.Fatalf("claimed attempt_count=%d want=1", claimed[0].AttemptCount)
	}

	if err := store.MarkWebhookOutboxDelivered(ctx, claimed[0].ID); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	claimed, err = store.ClaimPendingWebhookDeliveries(ctx, now, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("claim second delivery: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed second len=%d want=1", len(claimed))
	}
	if claimed[0].EventType != "message.created" {
		t.Fatalf("claimed second event_type=%s want=message.created", claimed[0].EventType)
	}

	retryAt := now.Add(30 * time.Second)
	if err := store.MarkWebhookOutboxPendingRetry(ctx, claimed[0].ID, retryAt, "temporary failure"); err != nil {
		t.Fatalf("mark pending retry: %v", err)
	}

	claimed, err = store.ClaimPendingWebhookDeliveries(ctx, now, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("claim before retry time: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed before retry len=%d want=0", len(claimed))
	}

	claimed, err = store.ClaimPendingWebhookDeliveries(ctx, retryAt.Add(time.Second), now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("claim after retry time: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed after retry len=%d want=1", len(claimed))
	}
	if claimed[0].AttemptCount != 2 {
		t.Fatalf("claimed retry attempt_count=%d want=2", claimed[0].AttemptCount)
	}

	if err := store.MarkWebhookOutboxDeadLetter(ctx, claimed[0].ID, "permanent failure"); err != nil {
		t.Fatalf("mark dead letter: %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM webhook_outbox WHERE id = $1`, claimed[0].ID).Scan(&status); err != nil {
		t.Fatalf("query dead letter status: %v", err)
	}
	if status != "dead_letter" {
		t.Fatalf("status=%s want=dead_letter", status)
	}
}

func TestRoomEventStreamLeaseLifecycle(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)
	now := time.Now().UTC()

	first, err := store.AcquireRoomEventStreamLease(ctx, repository.AcquireRoomEventStreamLeaseInput{
		LeaseID:                 "lease_1",
		RoomID:                  roomID,
		AgentID:                 "agt_a",
		RemoteIP:                "127.0.0.1",
		Now:                     now,
		LeaseExpiresAt:          now.Add(2 * time.Minute),
		MaxActivePerRoomAgent:   1,
		MaxConnectsPerMinuteKey: 5,
		MaxConnectsPerMinuteIP:  5,
	})
	if err != nil {
		t.Fatalf("acquire lease_1: %v", err)
	}
	if !first.Acquired {
		t.Fatalf("lease_1 denied reason=%s", first.DeniedReason)
	}

	second, err := store.AcquireRoomEventStreamLease(ctx, repository.AcquireRoomEventStreamLeaseInput{
		LeaseID:                 "lease_2",
		RoomID:                  roomID,
		AgentID:                 "agt_a",
		RemoteIP:                "127.0.0.1",
		Now:                     now,
		LeaseExpiresAt:          now.Add(2 * time.Minute),
		MaxActivePerRoomAgent:   1,
		MaxConnectsPerMinuteKey: 5,
		MaxConnectsPerMinuteIP:  5,
	})
	if err != nil {
		t.Fatalf("acquire lease_2: %v", err)
	}
	if second.Acquired {
		t.Fatalf("expected second lease denied, got acquired=%v", second.Acquired)
	}
	if second.DeniedReason != "max_active_streams_per_agent_room" {
		t.Fatalf("deny reason=%q want=max_active_streams_per_agent_room", second.DeniedReason)
	}

	if err := store.ReleaseRoomEventStreamLease(ctx, "lease_1"); err != nil {
		t.Fatalf("release lease_1: %v", err)
	}

	third, err := store.AcquireRoomEventStreamLease(ctx, repository.AcquireRoomEventStreamLeaseInput{
		LeaseID:                 "lease_3",
		RoomID:                  roomID,
		AgentID:                 "agt_a",
		RemoteIP:                "127.0.0.1",
		Now:                     now,
		LeaseExpiresAt:          now.Add(2 * time.Minute),
		MaxActivePerRoomAgent:   1,
		MaxConnectsPerMinuteKey: 5,
		MaxConnectsPerMinuteIP:  5,
	})
	if err != nil {
		t.Fatalf("acquire lease_3: %v", err)
	}
	if !third.Acquired {
		t.Fatalf("lease_3 denied reason=%s", third.DeniedReason)
	}
}

func TestMessageCoordinationState(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)
	if _, err := store.AppendMessage(ctx, repository.AppendMessageInput{
		ID:         "msg_coord_1",
		RoomID:     roomID,
		SenderID:   "agt_a",
		Turn:       0,
		Ciphertext: "hello",
	}); err != nil {
		t.Fatalf("append message: %v", err)
	}

	now := time.Now().UTC()
	err := store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
		roomLocker, ok := tx.(repository.RoomLockStore)
		if !ok {
			t.Fatal("tx store missing room lock support")
		}
		if err := roomLocker.LockRoom(ctx, roomID); err != nil {
			return err
		}
		counter, ok := tx.(repository.MessageCounterStore)
		if !ok {
			t.Fatal("tx store missing message counter support")
		}
		roomCount, err := counter.CountMessagesByRoomSince(ctx, roomID, now.Add(-time.Minute))
		if err != nil {
			return err
		}
		if roomCount != 1 {
			t.Fatalf("roomCount=%d want=1", roomCount)
		}
		senderCount, err := counter.CountMessagesBySenderSince(ctx, "agt_a", now.Add(-time.Minute))
		if err != nil {
			return err
		}
		if senderCount != 1 {
			t.Fatalf("senderCount=%d want=1", senderCount)
		}
		policy, ok := tx.(repository.AgentPolicyStore)
		if !ok {
			t.Fatal("tx store missing policy support")
		}
		blockedUntil, blocked, err := policy.GetAgentPolicyBlock(ctx, "agt_a", now)
		if err != nil {
			return err
		}
		if blocked {
			t.Fatalf("unexpected block at %v", blockedUntil)
		}
		state, err := policy.RecordAgentPolicyViolation(ctx, "agt_a", now, 5*time.Minute, 15*time.Minute, 3)
		if err != nil {
			return err
		}
		if state.ViolationCount != 1 {
			t.Fatalf("violation count after first record=%d want=1", state.ViolationCount)
		}
		state, err = policy.RecordAgentPolicyViolation(ctx, "agt_a", now, 5*time.Minute, 15*time.Minute, 3)
		if err != nil {
			return err
		}
		if state.ViolationCount != 2 || state.BlockedUntil != nil {
			t.Fatalf("state after second record=%+v", state)
		}
		state, err = policy.RecordAgentPolicyViolation(ctx, "agt_a", now, 5*time.Minute, 15*time.Minute, 3)
		if err != nil {
			return err
		}
		if state.ViolationCount != 3 || state.BlockedUntil == nil {
			t.Fatalf("state after third record=%+v", state)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("coordination tx: %v", err)
	}
}

func TestWebhookOutboxClaimIndependentPerEndpoint(t *testing.T) {
	db := openTestPostgresDB(t)
	defer db.Close()
	applyStoreMigrationsForTest(t, db)

	store := NewStore(db)
	ctx := context.Background()
	roomID := seedRoomForEvents(t, ctx, store)

	endpointA, err := store.CreateAgentWebhookEndpoint(ctx, repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_multi_a",
		AgentID:          "agt_b",
		URL:              "https://example.com/hooks/multi-a",
		SecretCiphertext: "enc-multi-a",
		KeyID:            "key-multi-a",
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create endpoint A: %v", err)
	}
	endpointB, err := store.CreateAgentWebhookEndpoint(ctx, repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_multi_b",
		AgentID:          "agt_b",
		URL:              "https://example.com/hooks/multi-b",
		SecretCiphertext: "enc-multi-b",
		KeyID:            "key-multi-b",
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create endpoint B: %v", err)
	}

	ev1, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{RoomID: roomID, EventType: "room.joined"})
	if err != nil {
		t.Fatalf("append room event 1: %v", err)
	}
	ev2, err := store.AppendRoomEvent(ctx, repository.AppendRoomEventInput{RoomID: roomID, EventType: "message.created"})
	if err != nil {
		t.Fatalf("append room event 2: %v", err)
	}

	now := time.Now().UTC()
	outboxA, err := store.CreateWebhookOutbox(ctx, repository.CreateWebhookOutboxInput{
		RoomID:        roomID,
		RoomEventID:   ev1.ID,
		TargetAgentID: "agt_b",
		EndpointID:    endpointA.ID,
		EventType:     "room.joined",
		Payload:       json.RawMessage(`{"delivery_id":"whd_multi_a_1"}`),
	})
	if err != nil {
		t.Fatalf("create outbox A: %v", err)
	}
	if _, err := store.CreateWebhookOutbox(ctx, repository.CreateWebhookOutboxInput{
		RoomID:        roomID,
		RoomEventID:   ev2.ID,
		TargetAgentID: "agt_b",
		EndpointID:    endpointB.ID,
		EventType:     "message.created",
		Payload:       json.RawMessage(`{"delivery_id":"whd_multi_b_1"}`),
	}); err != nil {
		t.Fatalf("create outbox B: %v", err)
	}

	retryAt := now.Add(5 * time.Minute)
	if err := store.MarkWebhookOutboxPendingRetry(ctx, outboxA.ID, retryAt, "endpoint a blocked"); err != nil {
		t.Fatalf("mark endpoint A pending retry: %v", err)
	}

	claimed, err := store.ClaimPendingWebhookDeliveries(ctx, now, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("claim deliveries: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed len=%d want=1", len(claimed))
	}
	if claimed[0].EndpointID != endpointB.ID {
		t.Fatalf("claimed endpoint_id=%s want=%s", claimed[0].EndpointID, endpointB.ID)
	}
}
