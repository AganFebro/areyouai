package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/febrian/areyouai/internal/domain"
	"github.com/febrian/areyouai/internal/repository"
	"github.com/lib/pq"
)

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) SupportsMultiInstanceCoordination() bool {
	return true
}

func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx repository.TxStore) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	wrapped := &txStore{tx: tx}
	if err := fn(ctx, wrapped); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateAgent(ctx context.Context, in repository.CreateAgentInput) (repository.Agent, error) {
	const q = `
INSERT INTO agents (id, name, api_key_hash)
VALUES ($1, $2, $3)
RETURNING id, name, api_key_hash, created_at`
	return scanAgent(s.db.QueryRowContext(ctx, q, in.ID, in.Name, in.APIKeyHash))
}

func (s *Store) FindAgentByAPIKeyHash(ctx context.Context, apiKeyHash string) (repository.Agent, error) {
	const q = `
SELECT id, name, api_key_hash, created_at
FROM agents
WHERE api_key_hash = $1`
	return scanAgent(s.db.QueryRowContext(ctx, q, apiKeyHash))
}

func (s *Store) CreateSession(ctx context.Context, in repository.CreateSessionInput) (repository.Session, error) {
	const q = `
INSERT INTO agent_sessions (token, agent_id, expires_at)
VALUES ($1, $2, $3)
RETURNING token, agent_id, created_at, expires_at`
	return scanSession(s.db.QueryRowContext(ctx, q, in.Token, in.AgentID, in.ExpiresAt))
}

func (s *Store) FindSession(ctx context.Context, token string) (repository.Session, error) {
	const q = `
SELECT token, agent_id, created_at, expires_at
FROM agent_sessions
WHERE token = $1`
	return scanSession(s.db.QueryRowContext(ctx, q, token))
}

func (s *Store) CreateAgentWebhookEndpoint(ctx context.Context, in repository.CreateAgentWebhookEndpointInput) (repository.AgentWebhookEndpoint, error) {
	const q = `
INSERT INTO agent_webhook_endpoints (id, agent_id, url, secret_ciphertext, key_id, enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, agent_id, url, secret_ciphertext, key_id, enabled, created_at, updated_at`
	return scanAgentWebhookEndpoint(s.db.QueryRowContext(
		ctx,
		q,
		in.ID,
		in.AgentID,
		in.URL,
		in.SecretCiphertext,
		in.KeyID,
		in.Enabled,
	))
}

func (s *Store) ListAgentWebhookEndpoints(ctx context.Context, agentID string) ([]repository.AgentWebhookEndpoint, error) {
	const q = `
SELECT id, agent_id, url, secret_ciphertext, key_id, enabled, created_at, updated_at
FROM agent_webhook_endpoints
WHERE agent_id = $1
ORDER BY created_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.AgentWebhookEndpoint
	for rows.Next() {
		item, scanErr := scanAgentWebhookEndpoint(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAgentWebhookEndpoint(ctx context.Context, agentID, endpointID string) error {
	const q = `
WITH deleted_outbox AS (
  DELETE FROM webhook_outbox
  WHERE endpoint_id = $1
),
deleted_endpoint AS (
  DELETE FROM agent_webhook_endpoints
  WHERE id = $1
    AND agent_id = $2
  RETURNING id
)
SELECT COUNT(1) FROM deleted_endpoint`
	var deleted int
	if err := s.db.QueryRowContext(ctx, q, endpointID, agentID).Scan(&deleted); err != nil {
		return err
	}
	if deleted == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (s *Store) CreateAgentStreamDelivery(ctx context.Context, in repository.CreateAgentStreamDeliveryInput) (repository.AgentStreamDelivery, error) {
	return createAgentStreamDelivery(ctx, s.db, in)
}

func (s *Store) GetAgentStreamDelivery(ctx context.Context, agentID, deliveryID string) (repository.AgentStreamDelivery, error) {
	const q = `
SELECT seq, delivery_id, agent_id, room_id, type, reason, payload, status, acked_at, expires_at, created_at
FROM agent_stream_deliveries
WHERE agent_id = $1 AND delivery_id = $2`
	return scanAgentStreamDelivery(s.db.QueryRowContext(ctx, q, agentID, deliveryID))
}

func (s *Store) ListPendingAgentStreamDeliveries(ctx context.Context, agentID string, afterSeq int64, now time.Time, limit int) ([]repository.AgentStreamDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	const q = `
SELECT seq, delivery_id, agent_id, room_id, type, reason, payload, status, acked_at, expires_at, created_at
FROM agent_stream_deliveries
WHERE agent_id = $1
  AND seq > $2
  AND status = 'pending'
  AND expires_at > $3
ORDER BY seq ASC
LIMIT $4`
	rows, err := s.db.QueryContext(ctx, q, agentID, afterSeq, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.AgentStreamDelivery
	for rows.Next() {
		item, scanErr := scanAgentStreamDelivery(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) AckAgentStreamDelivery(ctx context.Context, agentID, deliveryID string, ackedAt time.Time) error {
	const q = `
UPDATE agent_stream_deliveries
SET status = 'acked',
    acked_at = COALESCE(acked_at, $3)
WHERE agent_id = $1
  AND delivery_id = $2`
	res, err := s.db.ExecContext(ctx, q, agentID, deliveryID, ackedAt)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (s *Store) CreateListing(ctx context.Context, in repository.CreateListingInput) (repository.Listing, error) {
	tagsJSON, err := marshalListingTags(in.Tags)
	if err != nil {
		return repository.Listing{}, err
	}
	const q = `
INSERT INTO chat_listings (id, agent_id, topic, tags, max_turns, ttl_seconds, connected, room_id)
VALUES ($1, $2, $3, $4::jsonb, $5, $6, FALSE, $7)
RETURNING id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at, room_id`
	return scanListing(s.db.QueryRowContext(ctx, q, in.ID, in.AgentID, in.Topic, tagsJSON, in.MaxTurns, in.TTLSeconds, nullableText(in.RoomID)))
}

func (s *Store) GetListing(ctx context.Context, listingID string) (repository.Listing, error) {
	const q = `
SELECT id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at, room_id
FROM chat_listings
WHERE id = $1`
	return scanListing(s.db.QueryRowContext(ctx, q, listingID))
}

func (s *Store) MarkListingConnected(ctx context.Context, listingID string) error {
	const q = `UPDATE chat_listings SET connected = TRUE WHERE id = $1`
	res, err := s.db.ExecContext(ctx, q, listingID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (s *Store) SearchListings(ctx context.Context, query string) ([]repository.Listing, error) {
	base := `
SELECT id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at, room_id
FROM chat_listings
WHERE connected = FALSE`
	args := []any{}
	if strings.TrimSpace(query) != "" {
		base += ` AND (
  LOWER(topic) LIKE LOWER($1)
  OR EXISTS (
    SELECT 1
    FROM jsonb_array_elements_text(
      CASE
        WHEN jsonb_typeof(tags) = 'array' THEN tags
        ELSE '[]'::jsonb
      END
    ) AS t(value)
    WHERE LOWER(t.value) LIKE LOWER($1)
  )
)`
		args = append(args, "%"+query+"%")
	}
	base += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, base, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.Listing
	for rows.Next() {
		item, err := scanListingRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListRecoverableRoomsForAgent(ctx context.Context, agentID string, since time.Time) ([]repository.Room, error) {
	const q = `
SELECT id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash, human_code_expires_at, message_key_ciphertext
FROM rooms
WHERE (agent_a_id = $1 OR agent_b_id = $1)
  AND (
    state = $3
    OR (closed_at IS NOT NULL AND closed_at >= $2)
    OR (purged_at IS NOT NULL AND purged_at >= $2)
  )
ORDER BY
  CASE WHEN state = $3 THEN 0 ELSE 1 END,
  COALESCE(purged_at, closed_at, created_at) DESC,
  id DESC`
	rows, err := s.db.QueryContext(ctx, q, agentID, since, string(domain.RoomStateActive))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.Room
	for rows.Next() {
		item, scanErr := scanRoom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) CreateRoom(ctx context.Context, in repository.CreateRoomInput) (repository.Room, error) {
	const q = `
	INSERT INTO rooms (id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, human_code_hash, human_code_expires_at, message_key_ciphertext)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	RETURNING id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash, human_code_expires_at, message_key_ciphertext`
	return scanRoom(
		s.db.QueryRowContext(ctx, q, in.ID, in.Topic, in.AgentAID, nullableText(in.AgentBID), string(in.State), in.TurnIndex, in.MaxTurns, in.TTLAt, in.HumanCodeHash, nullableTime(in.HumanCodeExpiresAt), nullableText(in.MessageKeyCiphertext)),
	)
}

func (s *Store) GetRoom(ctx context.Context, roomID string) (repository.Room, error) {
	const q = `
SELECT id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash, human_code_expires_at, message_key_ciphertext
FROM rooms
WHERE id = $1`
	return scanRoom(s.db.QueryRowContext(ctx, q, roomID))
}

func (s *Store) UpdateRoom(ctx context.Context, in repository.UpdateRoomInput) (repository.Room, error) {
	current, err := s.GetRoom(ctx, in.ID)
	if err != nil {
		return repository.Room{}, err
	}
	if in.State != nil {
		current.State = *in.State
	}
	if in.AgentBID != nil {
		current.AgentBID = *in.AgentBID
	}
	if in.TurnIndex != nil {
		current.TurnIndex = *in.TurnIndex
	}
	if in.ClosedAt != nil {
		current.ClosedAt = in.ClosedAt
	}
	if in.PurgedAt != nil {
		current.PurgedAt = in.PurgedAt
	}
	if in.MessageKeyCiphertext != nil {
		current.MessageKeyCiphertext = *in.MessageKeyCiphertext
	}
	if in.Topic != nil {
		current.Topic = *in.Topic
	}

	const q = `
UPDATE rooms
SET topic = COALESCE($2, topic),
    agent_b_id = $3,
    state = $4,
    turn_index = $5,
    closed_at = $6,
    purged_at = $7,
    message_key_ciphertext = $8
WHERE id = $1
RETURNING id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash, human_code_expires_at, message_key_ciphertext`
	return scanRoom(s.db.QueryRowContext(
		ctx,
		q,
		current.ID,
		nullableText(current.Topic),
		nullableText(current.AgentBID),
		string(current.State),
		current.TurnIndex,
		current.ClosedAt,
		current.PurgedAt,
		nullableText(current.MessageKeyCiphertext),
	))
}

func (s *Store) AppendMessage(ctx context.Context, in repository.AppendMessageInput) (repository.Message, error) {
	const q = `
INSERT INTO messages (id, room_id, sender_id, turn, ciphertext)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, room_id, sender_id, turn, ciphertext, created_at`
	return scanMessage(s.db.QueryRowContext(ctx, q, in.ID, in.RoomID, in.SenderID, in.Turn, in.Ciphertext))
}

func (s *Store) ListRoomMessages(ctx context.Context, roomID string) ([]repository.Message, error) {
	const q = `
SELECT m.id, m.room_id, m.sender_id, a.name, m.turn, m.ciphertext, m.created_at
FROM messages m
LEFT JOIN agents a ON a.id = m.sender_id
WHERE m.room_id = $1
ORDER BY turn ASC`
	rows, err := s.db.QueryContext(ctx, q, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.Message
	for rows.Next() {
		item, err := scanMessageRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListRecentRoomMessages(ctx context.Context, roomID string, limit int) ([]repository.Message, error) {
	if limit <= 0 {
		limit = 1
	}
	const q = `
SELECT id, room_id, sender_id, sender_name, turn, ciphertext, created_at
FROM (
	SELECT m.id, m.room_id, m.sender_id, a.name AS sender_name, m.turn, m.ciphertext, m.created_at
	FROM messages m
	LEFT JOIN agents a ON a.id = m.sender_id
	WHERE m.room_id = $1
	ORDER BY turn DESC
	LIMIT $2
) recent
ORDER BY turn ASC`
	rows, err := s.db.QueryContext(ctx, q, roomID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.Message
	for rows.Next() {
		item, err := scanMessageRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GetRoomContext(ctx context.Context, roomID string) (repository.RoomContextState, error) {
	const q = `
SELECT room_id, context, version, updated_at, created_at
FROM room_context_state
WHERE room_id = $1`
	return scanRoomContext(s.db.QueryRowContext(ctx, q, roomID))
}

func (s *Store) UpsertRoomContext(ctx context.Context, in repository.UpsertRoomContextInput) (repository.RoomContextState, error) {
	const q = `
INSERT INTO room_context_state (room_id, context, version)
VALUES ($1, $2::jsonb, $3)
ON CONFLICT (room_id) DO UPDATE
SET context = EXCLUDED.context,
    version = EXCLUDED.version,
    updated_at = NOW()
WHERE room_context_state.version = EXCLUDED.version - 1
RETURNING room_id, context, version, updated_at, created_at`
	out, err := scanRoomContext(s.db.QueryRowContext(ctx, q, in.RoomID, in.Context, in.Version))
	if errors.Is(err, repository.ErrNotFound) {
		return repository.RoomContextState{}, repository.ErrConflict
	}
	return out, err
}

func (s *Store) UpsertViewer(ctx context.Context, in repository.UpsertViewerInput) (repository.Viewer, error) {
	const q = `
INSERT INTO room_viewers (id, room_id, viewer_token, joined_at, last_heartbeat_at, left_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (viewer_token) DO UPDATE
SET last_heartbeat_at = EXCLUDED.last_heartbeat_at,
    left_at = EXCLUDED.left_at
RETURNING id, room_id, viewer_token, joined_at, last_heartbeat_at, left_at`
	return scanViewer(s.db.QueryRowContext(ctx, q, in.ID, in.RoomID, in.ViewerToken, in.JoinedAt, in.LastHeartbeatAt, in.LeftAt))
}

func (s *Store) GetViewer(ctx context.Context, viewerToken string) (repository.Viewer, error) {
	const q = `
SELECT id, room_id, viewer_token, joined_at, last_heartbeat_at, left_at
FROM room_viewers
WHERE viewer_token = $1`
	return scanViewer(s.db.QueryRowContext(ctx, q, viewerToken))
}

func (s *Store) CountActiveViewers(ctx context.Context, roomID string, activeSince time.Time) (int, error) {
	const q = `
SELECT COUNT(1)
FROM room_viewers
WHERE room_id = $1
  AND left_at IS NULL
  AND last_heartbeat_at >= $2`
	var count int
	err := s.db.QueryRowContext(ctx, q, roomID, activeSince).Scan(&count)
	return count, err
}

func (s *Store) AppendAuditEvent(ctx context.Context, in repository.AppendAuditEventInput) error {
	const q = `
INSERT INTO audit_events (room_id, event, meta, message_count)
VALUES ($1, $2, $3, $4)`
	_, err := s.db.ExecContext(ctx, q, in.RoomID, in.Event, in.Meta, in.MessageCount)
	return err
}

func (s *Store) AppendAPIRequestLog(ctx context.Context, in repository.AppendAPIRequestLogInput) error {
	const q = `
INSERT INTO api_request_logs (
  request_id,
  method,
  path,
  route_name,
  query,
  status_code,
  duration_ms,
  remote_ip,
  user_agent,
  bytes_written,
  auth_present
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	_, err := s.db.ExecContext(
		ctx,
		q,
		in.RequestID,
		in.Method,
		in.Path,
		in.RouteName,
		in.Query,
		in.StatusCode,
		in.DurationMS,
		in.RemoteIP,
		in.UserAgent,
		in.BytesWritten,
		in.AuthPresent,
	)
	return err
}

func (s *Store) AppendRoomEvent(ctx context.Context, in repository.AppendRoomEventInput) (repository.RoomEvent, error) {
	const q = `
WITH inserted AS (
  INSERT INTO room_events (room_id, event_type, message_id, turn, sender_id)
  VALUES ($1, $2, $3, $4, $5)
  RETURNING id, room_id, event_type, message_id, turn, sender_id, created_at
)
SELECT i.id, i.room_id, i.event_type, i.message_id, i.turn, i.sender_id, m.ciphertext, i.created_at
FROM inserted i
LEFT JOIN messages m ON m.id = i.message_id`
	out, err := scanRoomEvent(s.db.QueryRowContext(
		ctx,
		q,
		in.RoomID,
		in.EventType,
		in.MessageID,
		in.Turn,
		in.SenderID,
	))
	if err != nil {
		return repository.RoomEvent{}, err
	}
	if out.Ciphertext == nil && in.Ciphertext != nil {
		out.Ciphertext = in.Ciphertext
	}
	return out, nil
}

func (s *Store) GetRoomEvent(ctx context.Context, eventID int64) (repository.RoomEvent, error) {
	const q = `
SELECT re.id, re.room_id, re.event_type, re.message_id, re.turn, re.sender_id, m.ciphertext, re.created_at
FROM room_events re
LEFT JOIN messages m ON m.id = re.message_id
WHERE re.id = $1`
	return scanRoomEvent(s.db.QueryRowContext(ctx, q, eventID))
}

func (s *Store) ListRoomEvents(ctx context.Context, in repository.ListRoomEventsInput) ([]repository.RoomEvent, error) {
	limit := in.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	const q = `
SELECT re.id, re.room_id, re.event_type, re.message_id, re.turn, re.sender_id, m.ciphertext, re.created_at
FROM room_events re
LEFT JOIN messages m ON m.id = re.message_id
WHERE re.room_id = $1
  AND re.id > $2
ORDER BY re.id ASC
LIMIT $3`
	rows, err := s.db.QueryContext(ctx, q, in.RoomID, in.SinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.RoomEvent
	for rows.Next() {
		item, scanErr := scanRoomEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) CreateWebhookOutbox(ctx context.Context, in repository.CreateWebhookOutboxInput) (repository.WebhookOutboxItem, error) {
	return createWebhookOutbox(ctx, s.db, in)
}

func (s *Store) ClaimPendingWebhookDeliveries(ctx context.Context, now, reclaimBefore time.Time, limit int) ([]repository.ClaimedWebhookDelivery, error) {
	return claimPendingWebhookDeliveries(ctx, s.db, now, reclaimBefore, limit)
}

func (s *Store) AcquireRoomEventStreamLease(ctx context.Context, in repository.AcquireRoomEventStreamLeaseInput) (repository.AcquireRoomEventStreamLeaseResult, error) {
	in.LeaseID = strings.TrimSpace(in.LeaseID)
	in.RoomID = strings.TrimSpace(in.RoomID)
	in.AgentID = strings.TrimSpace(in.AgentID)
	in.RemoteIP = strings.TrimSpace(in.RemoteIP)
	if in.LeaseID == "" || in.RoomID == "" || in.AgentID == "" {
		return repository.AcquireRoomEventStreamLeaseResult{}, fmt.Errorf("invalid stream lease input")
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	if in.LeaseExpiresAt.IsZero() || !in.LeaseExpiresAt.After(in.Now) {
		in.LeaseExpiresAt = in.Now.Add(90 * time.Second)
	}
	if in.MaxActivePerRoomAgent <= 0 {
		in.MaxActivePerRoomAgent = 5
	}
	if in.MaxConnectsPerMinuteKey <= 0 {
		in.MaxConnectsPerMinuteKey = 30
	}
	if in.MaxConnectsPerMinuteIP <= 0 {
		in.MaxConnectsPerMinuteIP = 120
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	roomAgentKey := fmt.Sprintf("stream_room_agent|%s|%s", in.RoomID, in.AgentID)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, roomAgentKey); err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}
	if in.RemoteIP != "" {
		ipKey := fmt.Sprintf("stream_ip|%s", in.RemoteIP)
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, ipKey); err != nil {
			return repository.AcquireRoomEventStreamLeaseResult{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM room_event_stream_leases WHERE expires_at <= $1`, in.Now); err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM room_event_stream_open_events WHERE opened_at <= $1`, in.Now.Add(-2*time.Minute)); err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}

	out := repository.AcquireRoomEventStreamLeaseResult{}
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(1) FROM room_event_stream_leases WHERE room_id = $1 AND agent_id = $2 AND expires_at > $3`,
		in.RoomID,
		in.AgentID,
		in.Now,
	).Scan(&out.ActivePerRoomAgent); err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}
	if out.ActivePerRoomAgent >= in.MaxActivePerRoomAgent {
		out.DeniedReason = "max_active_streams_per_agent_room"
		return out, nil
	}

	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(1) FROM room_event_stream_open_events WHERE room_id = $1 AND agent_id = $2 AND opened_at > $3`,
		in.RoomID,
		in.AgentID,
		in.Now.Add(-1*time.Minute),
	).Scan(&out.ConnectsPerMinuteKey); err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}
	if out.ConnectsPerMinuteKey >= in.MaxConnectsPerMinuteKey {
		out.DeniedReason = "reconnect_rate_limited_room_agent"
		return out, nil
	}

	if in.RemoteIP != "" {
		if err := tx.QueryRowContext(
			ctx,
			`SELECT COUNT(1) FROM room_event_stream_open_events WHERE remote_ip = $1 AND opened_at > $2`,
			in.RemoteIP,
			in.Now.Add(-1*time.Minute),
		).Scan(&out.ConnectsPerMinuteIP); err != nil {
			return repository.AcquireRoomEventStreamLeaseResult{}, err
		}
		if out.ConnectsPerMinuteIP >= in.MaxConnectsPerMinuteIP {
			out.DeniedReason = "reconnect_rate_limited_ip"
			return out, nil
		}
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO room_event_stream_leases (lease_id, room_id, agent_id, remote_ip, opened_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		in.LeaseID,
		in.RoomID,
		in.AgentID,
		in.RemoteIP,
		in.Now,
		in.LeaseExpiresAt,
	); err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO room_event_stream_open_events (room_id, agent_id, remote_ip, opened_at) VALUES ($1, $2, $3, $4)`,
		in.RoomID,
		in.AgentID,
		in.RemoteIP,
		in.Now,
	); err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return repository.AcquireRoomEventStreamLeaseResult{}, err
	}
	committed = true

	out.Acquired = true
	out.ActivePerRoomAgent++
	out.ConnectsPerMinuteKey++
	if in.RemoteIP != "" {
		out.ConnectsPerMinuteIP++
	}
	return out, nil
}

func (s *Store) ReleaseRoomEventStreamLease(ctx context.Context, leaseID string) error {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM room_event_stream_leases WHERE lease_id = $1`, leaseID)
	return err
}

func (s *Store) MarkWebhookOutboxDelivered(ctx context.Context, id int64) error {
	const q = `
UPDATE webhook_outbox
SET status = 'delivered',
    last_error = '',
    updated_at = NOW()
WHERE id = $1`
	_, err := s.db.ExecContext(ctx, q, id)
	return err
}

func (s *Store) MarkWebhookOutboxPendingRetry(ctx context.Context, id int64, nextAttemptAt time.Time, lastError string) error {
	const q = `
UPDATE webhook_outbox
SET status = 'pending',
    next_attempt_at = $2,
    last_error = $3,
    updated_at = NOW()
WHERE id = $1`
	_, err := s.db.ExecContext(ctx, q, id, nextAttemptAt, lastError)
	return err
}

func (s *Store) MarkWebhookOutboxDeadLetter(ctx context.Context, id int64, lastError string) error {
	const q = `
UPDATE webhook_outbox
SET status = 'dead_letter',
    last_error = $2,
    updated_at = NOW()
WHERE id = $1`
	_, err := s.db.ExecContext(ctx, q, id, lastError)
	return err
}

func (s *Store) CreateRoomScopedToken(ctx context.Context, in repository.CreateRoomScopedTokenInput) (repository.RoomScopedToken, error) {
	const q = `
INSERT INTO room_scoped_tokens (id, room_id, agent_id, token_hash, scope, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, room_id, agent_id, token_hash, scope, expires_at, revoked_at, created_at`
	return scanRoomScopedToken(s.db.QueryRowContext(
		ctx,
		q,
		in.ID,
		in.RoomID,
		in.AgentID,
		in.TokenHash,
		in.Scope,
		in.ExpiresAt,
	))
}

func (s *Store) FindRoomScopedTokenByHash(ctx context.Context, tokenHash string) (repository.RoomScopedToken, error) {
	const q = `
SELECT id, room_id, agent_id, token_hash, scope, expires_at, revoked_at, created_at
FROM room_scoped_tokens
WHERE token_hash = $1`
	return scanRoomScopedToken(s.db.QueryRowContext(ctx, q, tokenHash))
}

func (s *Store) TouchRoomScopedToken(ctx context.Context, tokenHash string, lastUsedAt, expiresAt time.Time) error {
	const q = `
UPDATE room_scoped_tokens
SET expires_at = $2
WHERE token_hash = $1
  AND revoked_at IS NULL`
	_, err := s.db.ExecContext(ctx, q, tokenHash, expiresAt)
	return err
}

func (s *Store) RevokeRoomScopedTokens(ctx context.Context, roomID, agentID string, revokedAt time.Time) error {
	const q = `
UPDATE room_scoped_tokens
SET revoked_at = $3
WHERE room_id = $1
  AND agent_id = $2
  AND revoked_at IS NULL`
	_, err := s.db.ExecContext(ctx, q, roomID, agentID, revokedAt)
	return err
}

func (s *Store) PurgeRoomContent(ctx context.Context, roomID string, purgedAt time.Time) error {
	return purgeRoomContentExec(ctx, s.db, roomID, purgedAt)
}

func (s *Store) LockAdvisory(ctx context.Context, key string) error {
	return lockAdvisoryExec(ctx, s.db, key)
}

func (s *Store) ListRoomsForLifecycleSweep(ctx context.Context, now time.Time, limit int) ([]repository.Room, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	const q = `
SELECT id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash, human_code_expires_at, message_key_ciphertext
FROM rooms
WHERE state = $1
   OR (state IN ($2, $3) AND ttl_at <= $4)
ORDER BY COALESCE(closed_at, ttl_at, created_at) ASC, id ASC
LIMIT $5`
	rows, err := s.db.QueryContext(ctx, q, string(domain.RoomStateClosed), string(domain.RoomStateOpen), string(domain.RoomStateActive), now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.Room
	for rows.Next() {
		item, scanErr := scanRoom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func purgeRoomContentExec(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, roomID string, purgedAt time.Time) error {
	const deleteMsgs = `DELETE FROM messages WHERE room_id = $1`
	if _, err := exec.ExecContext(ctx, deleteMsgs, roomID); err != nil {
		return err
	}
	const clearContext = `DELETE FROM room_context_state WHERE room_id = $1`
	if _, err := exec.ExecContext(ctx, clearContext, roomID); err != nil {
		return err
	}
	const clearViewers = `DELETE FROM room_viewers WHERE room_id = $1`
	if _, err := exec.ExecContext(ctx, clearViewers, roomID); err != nil {
		return err
	}
	const clearEvents = `DELETE FROM room_events WHERE room_id = $1`
	if _, err := exec.ExecContext(ctx, clearEvents, roomID); err != nil {
		return err
	}
	const updateRoom = `UPDATE rooms SET state = $2, purged_at = $3 WHERE id = $1`
	_, err := exec.ExecContext(ctx, updateRoom, roomID, string(domain.RoomStatePurged), purgedAt)
	return err
}

func lockAdvisoryExec(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("invalid advisory lock key")
	}
	_, err := exec.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockKey(key))
	return err
}

func advisoryLockKey(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int64(h.Sum64())
}

func (s *Store) GetAdminOverview(ctx context.Context, now time.Time) (repository.AdminOverview, error) {
	var out repository.AdminOverview

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM agents`).Scan(&out.AgentsTotal); err != nil {
		return repository.AdminOverview{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM agent_sessions WHERE expires_at IS NOT NULL AND expires_at > $1`, now).Scan(&out.SessionsActive); err != nil {
		return repository.AdminOverview{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM rooms WHERE state = $1`, string(domain.RoomStateOpen)).Scan(&out.RoomsOpen); err != nil {
		return repository.AdminOverview{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM rooms WHERE state = $1`, string(domain.RoomStateActive)).Scan(&out.RoomsActive); err != nil {
		return repository.AdminOverview{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM rooms WHERE state = $1`, string(domain.RoomStateClosed)).Scan(&out.RoomsClosed); err != nil {
		return repository.AdminOverview{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM rooms WHERE state = $1`, string(domain.RoomStatePurged)).Scan(&out.RoomsPurged); err != nil {
		return repository.AdminOverview{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM messages`).Scan(&out.MessagesTotal); err != nil {
		return repository.AdminOverview{}, err
	}
	return out, nil
}

func (s *Store) ListAdminRooms(ctx context.Context, limit int) ([]repository.AdminRoom, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	const q = `
SELECT r.id, r.agent_a_id, COALESCE(a.name, ''), COALESCE(r.agent_b_id, ''), COALESCE(b.name, ''), r.state, r.turn_index, r.max_turns, r.ttl_at, r.created_at, r.closed_at, r.purged_at
FROM rooms r
LEFT JOIN agents a ON a.id = r.agent_a_id
LEFT JOIN agents b ON b.id = r.agent_b_id
ORDER BY r.created_at DESC
LIMIT $1`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.AdminRoom
	for rows.Next() {
		item, scanErr := scanAdminRoom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListAuditEvents(ctx context.Context, limit int) ([]repository.AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	const q = `
SELECT id, room_id, event, meta, message_count, created_at
FROM audit_events
ORDER BY id DESC
LIMIT $1`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.AuditEvent
	for rows.Next() {
		item, scanErr := scanAuditEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type txStore struct {
	tx *sql.Tx
}

func (s *txStore) CreateListing(ctx context.Context, in repository.CreateListingInput) (repository.Listing, error) {
	tagsJSON, err := marshalListingTags(in.Tags)
	if err != nil {
		return repository.Listing{}, err
	}
	const q = `
INSERT INTO chat_listings (id, agent_id, topic, tags, max_turns, ttl_seconds, connected, room_id)
VALUES ($1, $2, $3, $4::jsonb, $5, $6, FALSE, $7)
RETURNING id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at, room_id`
	return scanListing(s.tx.QueryRowContext(ctx, q, in.ID, in.AgentID, in.Topic, tagsJSON, in.MaxTurns, in.TTLSeconds, nullableText(in.RoomID)))
}

func (s *txStore) GetListing(ctx context.Context, listingID string) (repository.Listing, error) {
	const q = `
SELECT id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at, room_id
FROM chat_listings
WHERE id = $1`
	return scanListing(s.tx.QueryRowContext(ctx, q, listingID))
}

func (s *txStore) MarkListingConnected(ctx context.Context, listingID string) error {
	const q = `UPDATE chat_listings SET connected = TRUE WHERE id = $1 AND connected = FALSE`
	res, err := s.tx.ExecContext(ctx, q, listingID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (s *txStore) CreateRoom(ctx context.Context, in repository.CreateRoomInput) (repository.Room, error) {
	const q = `
	INSERT INTO rooms (id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, human_code_hash, human_code_expires_at, message_key_ciphertext)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	RETURNING id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash, human_code_expires_at, message_key_ciphertext`
	return scanRoom(
		s.tx.QueryRowContext(ctx, q, in.ID, in.Topic, in.AgentAID, nullableText(in.AgentBID), string(in.State), in.TurnIndex, in.MaxTurns, in.TTLAt, in.HumanCodeHash, nullableTime(in.HumanCodeExpiresAt), nullableText(in.MessageKeyCiphertext)),
	)
}

func (s *txStore) GetRoom(ctx context.Context, roomID string) (repository.Room, error) {
	const q = `
SELECT id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash, human_code_expires_at, message_key_ciphertext
FROM rooms
WHERE id = $1`
	return scanRoom(s.tx.QueryRowContext(ctx, q, roomID))
}

func (s *txStore) UpdateRoom(ctx context.Context, in repository.UpdateRoomInput) (repository.Room, error) {
	current, err := s.GetRoom(ctx, in.ID)
	if err != nil {
		return repository.Room{}, err
	}
	if in.State != nil {
		current.State = *in.State
	}
	if in.AgentBID != nil {
		current.AgentBID = *in.AgentBID
	}
	if in.TurnIndex != nil {
		current.TurnIndex = *in.TurnIndex
	}
	if in.ClosedAt != nil {
		current.ClosedAt = in.ClosedAt
	}
	if in.PurgedAt != nil {
		current.PurgedAt = in.PurgedAt
	}
	if in.MessageKeyCiphertext != nil {
		current.MessageKeyCiphertext = *in.MessageKeyCiphertext
	}
	if in.Topic != nil {
		current.Topic = *in.Topic
	}

	const q = `
UPDATE rooms
SET topic = COALESCE($2, topic),
    agent_b_id = $3,
    state = $4,
    turn_index = $5,
    closed_at = $6,
    purged_at = $7,
    message_key_ciphertext = $8
WHERE id = $1
RETURNING id, topic, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash, human_code_expires_at, message_key_ciphertext`
	return scanRoom(s.tx.QueryRowContext(
		ctx,
		q,
		current.ID,
		nullableText(current.Topic),
		nullableText(current.AgentBID),
		string(current.State),
		current.TurnIndex,
		current.ClosedAt,
		current.PurgedAt,
		nullableText(current.MessageKeyCiphertext),
	))
}

func (s *txStore) AppendMessage(ctx context.Context, in repository.AppendMessageInput) (repository.Message, error) {
	const q = `
INSERT INTO messages (id, room_id, sender_id, turn, ciphertext)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, room_id, sender_id, turn, ciphertext, created_at`
	return scanMessage(s.tx.QueryRowContext(ctx, q, in.ID, in.RoomID, in.SenderID, in.Turn, in.Ciphertext))
}

func (s *txStore) PurgeRoomContent(ctx context.Context, roomID string, purgedAt time.Time) error {
	return purgeRoomContentExec(ctx, s.tx, roomID, purgedAt)
}

func (s *txStore) AppendRoomEvent(ctx context.Context, in repository.AppendRoomEventInput) (repository.RoomEvent, error) {
	const q = `
WITH inserted AS (
  INSERT INTO room_events (room_id, event_type, message_id, turn, sender_id)
  VALUES ($1, $2, $3, $4, $5)
  RETURNING id, room_id, event_type, message_id, turn, sender_id, created_at
)
SELECT i.id, i.room_id, i.event_type, i.message_id, i.turn, i.sender_id, m.ciphertext, i.created_at
FROM inserted i
LEFT JOIN messages m ON m.id = i.message_id`
	out, err := scanRoomEvent(s.tx.QueryRowContext(
		ctx,
		q,
		in.RoomID,
		in.EventType,
		in.MessageID,
		in.Turn,
		in.SenderID,
	))
	if err != nil {
		return repository.RoomEvent{}, err
	}
	if out.Ciphertext == nil && in.Ciphertext != nil {
		out.Ciphertext = in.Ciphertext
	}
	return out, nil
}

func (s *txStore) CreateAgentStreamDelivery(ctx context.Context, in repository.CreateAgentStreamDeliveryInput) (repository.AgentStreamDelivery, error) {
	return createAgentStreamDelivery(ctx, s.tx, in)
}

func (s *txStore) ListAgentWebhookEndpoints(ctx context.Context, agentID string) ([]repository.AgentWebhookEndpoint, error) {
	const q = `
SELECT id, agent_id, url, secret_ciphertext, key_id, enabled, created_at, updated_at
FROM agent_webhook_endpoints
WHERE agent_id = $1
ORDER BY created_at DESC, id DESC`
	rows, err := s.tx.QueryContext(ctx, q, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.AgentWebhookEndpoint
	for rows.Next() {
		item, scanErr := scanAgentWebhookEndpoint(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *txStore) CreateWebhookOutbox(ctx context.Context, in repository.CreateWebhookOutboxInput) (repository.WebhookOutboxItem, error) {
	return createWebhookOutbox(ctx, s.tx, in)
}

func (s *txStore) CreateRoomScopedToken(ctx context.Context, in repository.CreateRoomScopedTokenInput) (repository.RoomScopedToken, error) {
	const q = `
INSERT INTO room_scoped_tokens (id, room_id, agent_id, token_hash, scope, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, room_id, agent_id, token_hash, scope, expires_at, revoked_at, created_at`
	return scanRoomScopedToken(s.tx.QueryRowContext(
		ctx,
		q,
		in.ID,
		in.RoomID,
		in.AgentID,
		in.TokenHash,
		in.Scope,
		in.ExpiresAt,
	))
}

func (s *txStore) RevokeRoomScopedTokens(ctx context.Context, roomID, agentID string, revokedAt time.Time) error {
	const q = `
UPDATE room_scoped_tokens
SET revoked_at = $3
WHERE room_id = $1
  AND agent_id = $2
  AND revoked_at IS NULL`
	_, err := s.tx.ExecContext(ctx, q, roomID, agentID, revokedAt)
	return err
}

func (s *txStore) LockRoom(ctx context.Context, roomID string) error {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return fmt.Errorf("invalid room lock key")
	}
	const q = `SELECT 1 FROM rooms WHERE id = $1 FOR UPDATE`
	var out int
	return normalizeErr(s.tx.QueryRowContext(ctx, q, roomID).Scan(&out))
}

func (s *txStore) LockAdvisory(ctx context.Context, key string) error {
	return lockAdvisoryExec(ctx, s.tx, key)
}

func (s *txStore) CountMessagesBySenderSince(ctx context.Context, senderID string, since time.Time) (int, error) {
	const q = `SELECT COUNT(1) FROM messages WHERE sender_id = $1 AND created_at >= $2`
	var count int
	err := s.tx.QueryRowContext(ctx, q, strings.TrimSpace(senderID), since).Scan(&count)
	return count, err
}

func (s *txStore) CountMessagesByRoomSince(ctx context.Context, roomID string, since time.Time) (int, error) {
	const q = `SELECT COUNT(1) FROM messages WHERE room_id = $1 AND created_at >= $2`
	var count int
	err := s.tx.QueryRowContext(ctx, q, strings.TrimSpace(roomID), since).Scan(&count)
	return count, err
}

func (s *txStore) GetAgentPolicyBlock(ctx context.Context, agentID string, now time.Time) (time.Time, bool, error) {
	const q = `SELECT blocked_until FROM agent_policy_state WHERE agent_id = $1`
	var blockedUntil sql.NullTime
	err := s.tx.QueryRowContext(ctx, q, strings.TrimSpace(agentID)).Scan(&blockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, normalizeErr(err)
	}
	if !blockedUntil.Valid {
		return time.Time{}, false, nil
	}
	if now.Before(blockedUntil.Time) {
		return blockedUntil.Time, true, nil
	}
	return time.Time{}, false, nil
}

func (s *txStore) RecordAgentPolicyViolation(ctx context.Context, agentID string, now time.Time, window time.Duration, blockDuration time.Duration, maxViolations int) (repository.PolicyState, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return repository.PolicyState{}, fmt.Errorf("invalid agent id")
	}
	if maxViolations <= 0 {
		maxViolations = 1
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	current := repository.PolicyState{}
	var windowStartedAt sql.NullTime
	var blockedUntil sql.NullTime
	var count int
	const selectQ = `
SELECT window_started_at, violation_count, blocked_until
FROM agent_policy_state
WHERE agent_id = $1`
	err := s.tx.QueryRowContext(ctx, selectQ, agentID).Scan(&windowStartedAt, &count, &blockedUntil)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return repository.PolicyState{}, normalizeErr(err)
	}
	if windowStartedAt.Valid && now.Sub(windowStartedAt.Time) > window {
		windowStartedAt = sql.NullTime{Time: now, Valid: true}
		count = 0
		blockedUntil = sql.NullTime{}
	}
	count++
	if !windowStartedAt.Valid {
		windowStartedAt = sql.NullTime{Time: now, Valid: true}
	}
	if count >= maxViolations && blockDuration > 0 {
		blockedUntil = sql.NullTime{Time: now.Add(blockDuration), Valid: true}
	}
	const upsertQ = `
INSERT INTO agent_policy_state (agent_id, window_started_at, violation_count, blocked_until, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (agent_id) DO UPDATE
SET window_started_at = EXCLUDED.window_started_at,
    violation_count = EXCLUDED.violation_count,
    blocked_until = EXCLUDED.blocked_until,
    updated_at = NOW()`
	if _, err := s.tx.ExecContext(ctx, upsertQ, agentID, windowStartedAt.Time, count, blockedUntil); err != nil {
		return repository.PolicyState{}, normalizeErr(err)
	}
	current.ViolationCount = count
	if blockedUntil.Valid {
		current.BlockedUntil = &blockedUntil.Time
	}
	return current, nil
}

func scanAgent(row interface{ Scan(dest ...any) error }) (repository.Agent, error) {
	var a repository.Agent
	err := row.Scan(&a.ID, &a.Name, &a.APIKeyHash, &a.CreatedAt)
	if err != nil {
		return repository.Agent{}, normalizeErr(err)
	}
	return a, nil
}

func scanSession(row interface{ Scan(dest ...any) error }) (repository.Session, error) {
	var s repository.Session
	err := row.Scan(&s.Token, &s.AgentID, &s.CreatedAt, &s.ExpiresAt)
	if err != nil {
		return repository.Session{}, normalizeErr(err)
	}
	return s, nil
}

func scanListing(row interface{ Scan(dest ...any) error }) (repository.Listing, error) {
	var raw []byte
	var l repository.Listing
	var roomID sql.NullString
	err := row.Scan(&l.ID, &l.AgentID, &l.Topic, &raw, &l.MaxTurns, &l.TTLSeconds, &l.Connected, &l.CreatedAt, &roomID)
	if err != nil {
		return repository.Listing{}, normalizeErr(err)
	}
	if roomID.Valid {
		l.RoomID = roomID.String
	}
	if len(raw) == 0 || string(raw) == "null" {
		l.Tags = []string{}
		return l, nil
	}
	if err := json.Unmarshal(raw, &l.Tags); err != nil {
		return repository.Listing{}, fmt.Errorf("decode tags: %w", err)
	}
	if l.Tags == nil {
		l.Tags = []string{}
	}
	return l, nil
}

func scanListingRows(rows *sql.Rows) (repository.Listing, error) {
	return scanListing(rows)
}

func marshalListingTags(tags []string) ([]byte, error) {
	if tags == nil {
		tags = []string{}
	}
	return json.Marshal(tags)
}

func scanRoom(row interface{ Scan(dest ...any) error }) (repository.Room, error) {
	var r repository.Room
	var state string
	var topic sql.NullString
	var agentBID sql.NullString
	var messageKeyCiphertext sql.NullString
	err := row.Scan(
		&r.ID,
		&topic,
		&r.AgentAID,
		&agentBID,
		&state,
		&r.TurnIndex,
		&r.MaxTurns,
		&r.TTLAt,
		&r.CreatedAt,
		&r.ClosedAt,
		&r.PurgedAt,
		&r.HumanCodeHash,
		&r.HumanCodeExpiresAt,
		&messageKeyCiphertext,
	)
	if err != nil {
		return repository.Room{}, normalizeErr(err)
	}
	if agentBID.Valid {
		r.AgentBID = agentBID.String
	}
	if topic.Valid {
		r.Topic = topic.String
	}
	r.State = domain.RoomState(state)
	if messageKeyCiphertext.Valid {
		r.MessageKeyCiphertext = messageKeyCiphertext.String
	}
	return r, nil
}

func scanMessage(row interface{ Scan(dest ...any) error }) (repository.Message, error) {
	var m repository.Message
	err := row.Scan(&m.ID, &m.RoomID, &m.SenderID, &m.Turn, &m.Ciphertext, &m.CreatedAt)
	if err != nil {
		return repository.Message{}, normalizeErr(err)
	}
	return m, nil
}

func scanMessageRows(rows *sql.Rows) (repository.Message, error) {
	var m repository.Message
	var senderName sql.NullString
	err := rows.Scan(&m.ID, &m.RoomID, &m.SenderID, &senderName, &m.Turn, &m.Ciphertext, &m.CreatedAt)
	if err != nil {
		return repository.Message{}, normalizeErr(err)
	}
	if senderName.Valid {
		m.SenderName = senderName.String
	}
	return m, nil
}

func scanViewer(row interface{ Scan(dest ...any) error }) (repository.Viewer, error) {
	var v repository.Viewer
	err := row.Scan(&v.ID, &v.RoomID, &v.ViewerToken, &v.JoinedAt, &v.LastHeartbeatAt, &v.LeftAt)
	if err != nil {
		return repository.Viewer{}, normalizeErr(err)
	}
	return v, nil
}

func scanRoomContext(row interface{ Scan(dest ...any) error }) (repository.RoomContextState, error) {
	var out repository.RoomContextState
	var raw []byte
	err := row.Scan(&out.RoomID, &raw, &out.Version, &out.UpdatedAt, &out.CreatedAt)
	if err != nil {
		return repository.RoomContextState{}, normalizeErr(err)
	}
	if len(raw) == 0 {
		out.Context = json.RawMessage(`{}`)
	} else {
		out.Context = json.RawMessage(raw)
	}
	return out, nil
}

func scanAdminRoom(row interface{ Scan(dest ...any) error }) (repository.AdminRoom, error) {
	var out repository.AdminRoom
	var state string
	err := row.Scan(
		&out.ID,
		&out.AgentAID,
		&out.AgentAName,
		&out.AgentBID,
		&out.AgentBName,
		&state,
		&out.TurnIndex,
		&out.MaxTurns,
		&out.TTLAt,
		&out.CreatedAt,
		&out.ClosedAt,
		&out.PurgedAt,
	)
	if err != nil {
		return repository.AdminRoom{}, normalizeErr(err)
	}
	out.State = domain.RoomState(state)
	return out, nil
}

func nullableText(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func scanAuditEvent(row interface{ Scan(dest ...any) error }) (repository.AuditEvent, error) {
	var out repository.AuditEvent
	err := row.Scan(&out.ID, &out.RoomID, &out.Event, &out.Meta, &out.MessageCount, &out.CreatedAt)
	if err != nil {
		return repository.AuditEvent{}, normalizeErr(err)
	}
	return out, nil
}

func scanAgentWebhookEndpoint(row interface{ Scan(dest ...any) error }) (repository.AgentWebhookEndpoint, error) {
	var out repository.AgentWebhookEndpoint
	err := row.Scan(
		&out.ID,
		&out.AgentID,
		&out.URL,
		&out.SecretCiphertext,
		&out.KeyID,
		&out.Enabled,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		return repository.AgentWebhookEndpoint{}, normalizeErr(err)
	}
	return out, nil
}

func scanRoomEvent(row interface{ Scan(dest ...any) error }) (repository.RoomEvent, error) {
	var out repository.RoomEvent
	var messageID, senderID, ciphertext sql.NullString
	var turn sql.NullInt64
	err := row.Scan(
		&out.ID,
		&out.RoomID,
		&out.EventType,
		&messageID,
		&turn,
		&senderID,
		&ciphertext,
		&out.CreatedAt,
	)
	if err != nil {
		return repository.RoomEvent{}, normalizeErr(err)
	}
	if messageID.Valid {
		out.MessageID = &messageID.String
	}
	if turn.Valid {
		v := int(turn.Int64)
		out.Turn = &v
	}
	if senderID.Valid {
		out.SenderID = &senderID.String
	}
	if ciphertext.Valid {
		out.Ciphertext = &ciphertext.String
	}
	return out, nil
}

func scanWebhookOutboxItem(row interface{ Scan(dest ...any) error }) (repository.WebhookOutboxItem, error) {
	var out repository.WebhookOutboxItem
	var payload []byte
	err := row.Scan(
		&out.ID,
		&out.RoomID,
		&out.RoomEventID,
		&out.TargetAgentID,
		&out.EndpointID,
		&out.EventType,
		&payload,
		&out.Status,
		&out.AttemptCount,
		&out.NextAttemptAt,
		&out.LastError,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		return repository.WebhookOutboxItem{}, normalizeErr(err)
	}
	if len(payload) == 0 {
		out.Payload = json.RawMessage(`{}`)
	} else {
		out.Payload = json.RawMessage(payload)
	}
	return out, nil
}

func scanClaimedWebhookDelivery(row interface{ Scan(dest ...any) error }) (repository.ClaimedWebhookDelivery, error) {
	var out repository.ClaimedWebhookDelivery
	var payload []byte
	err := row.Scan(
		&out.ID,
		&out.RoomID,
		&out.RoomEventID,
		&out.TargetAgentID,
		&out.EndpointID,
		&out.EventType,
		&payload,
		&out.Status,
		&out.AttemptCount,
		&out.NextAttemptAt,
		&out.LastError,
		&out.CreatedAt,
		&out.UpdatedAt,
		&out.EndpointURL,
		&out.EndpointSecretCiphertext,
		&out.EndpointKeyID,
		&out.EndpointEnabled,
	)
	if err != nil {
		return repository.ClaimedWebhookDelivery{}, normalizeErr(err)
	}
	if len(payload) == 0 {
		out.Payload = json.RawMessage(`{}`)
	} else {
		out.Payload = json.RawMessage(payload)
	}
	return out, nil
}

func scanRoomScopedToken(row interface{ Scan(dest ...any) error }) (repository.RoomScopedToken, error) {
	var out repository.RoomScopedToken
	err := row.Scan(
		&out.ID,
		&out.RoomID,
		&out.AgentID,
		&out.TokenHash,
		&out.Scope,
		&out.ExpiresAt,
		&out.RevokedAt,
		&out.CreatedAt,
	)
	if err != nil {
		return repository.RoomScopedToken{}, normalizeErr(err)
	}
	return out, nil
}

func scanAgentStreamDelivery(row interface{ Scan(dest ...any) error }) (repository.AgentStreamDelivery, error) {
	var out repository.AgentStreamDelivery
	var payload []byte
	err := row.Scan(
		&out.Seq,
		&out.DeliveryID,
		&out.AgentID,
		&out.RoomID,
		&out.Type,
		&out.Reason,
		&payload,
		&out.Status,
		&out.AckedAt,
		&out.ExpiresAt,
		&out.CreatedAt,
	)
	if err != nil {
		return repository.AgentStreamDelivery{}, normalizeErr(err)
	}
	if len(payload) == 0 {
		out.Payload = json.RawMessage(`{}`)
	} else {
		out.Payload = json.RawMessage(payload)
	}
	return out, nil
}

func createAgentStreamDelivery(ctx context.Context, exec interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, in repository.CreateAgentStreamDeliveryInput) (repository.AgentStreamDelivery, error) {
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "pending"
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	expiresAt := in.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(30 * time.Minute)
	}
	const q = `
INSERT INTO agent_stream_deliveries (
  delivery_id,
  agent_id,
  room_id,
  type,
  reason,
  payload,
  status,
  expires_at
)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
RETURNING seq, delivery_id, agent_id, room_id, type, reason, payload, status, acked_at, expires_at, created_at`
	return scanAgentStreamDelivery(exec.QueryRowContext(
		ctx,
		q,
		in.DeliveryID,
		in.AgentID,
		in.RoomID,
		in.Type,
		in.Reason,
		payload,
		status,
		expiresAt,
	))
}

func createWebhookOutbox(ctx context.Context, exec interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, in repository.CreateWebhookOutboxInput) (repository.WebhookOutboxItem, error) {
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "pending"
	}
	nextAttemptAt := in.NextAttemptAt
	if nextAttemptAt.IsZero() {
		nextAttemptAt = time.Now().UTC()
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	const q = `
INSERT INTO webhook_outbox (
  room_id,
  room_event_id,
  target_agent_id,
  endpoint_id,
  event_type,
  payload,
  status,
  attempt_count,
  next_attempt_at,
  last_error
)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10)
RETURNING id, room_id, room_event_id, target_agent_id, endpoint_id, event_type, payload, status, attempt_count, next_attempt_at, last_error, created_at, updated_at`
	return scanWebhookOutboxItem(exec.QueryRowContext(
		ctx,
		q,
		in.RoomID,
		in.RoomEventID,
		in.TargetAgentID,
		in.EndpointID,
		in.EventType,
		payload,
		status,
		in.AttemptCount,
		nextAttemptAt,
		in.LastError,
	))
}

func claimPendingWebhookDeliveries(ctx context.Context, exec interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, now, reclaimBefore time.Time, limit int) ([]repository.ClaimedWebhookDelivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	const q = `
WITH candidates AS (
  SELECT wo.id
  FROM webhook_outbox wo
  JOIN agent_webhook_endpoints ep ON ep.id = wo.endpoint_id
  WHERE (
    (wo.status = 'pending' AND wo.next_attempt_at <= $1)
    OR
    (wo.status = 'delivering' AND wo.updated_at <= $2)
  )
  AND NOT EXISTS (
    SELECT 1
    FROM webhook_outbox prev
    WHERE prev.room_id = wo.room_id
      AND prev.target_agent_id = wo.target_agent_id
      AND prev.endpoint_id = wo.endpoint_id
      AND prev.id < wo.id
      AND prev.status NOT IN ('delivered', 'dead_letter')
  )
  ORDER BY wo.id ASC
  LIMIT $3
  FOR UPDATE SKIP LOCKED
),
claimed AS (
  UPDATE webhook_outbox wo
  SET status = 'delivering',
      attempt_count = wo.attempt_count + 1,
      updated_at = NOW()
  FROM candidates c
  WHERE wo.id = c.id
  RETURNING wo.id, wo.room_id, wo.room_event_id, wo.target_agent_id, wo.endpoint_id, wo.event_type, wo.payload, wo.status, wo.attempt_count, wo.next_attempt_at, wo.last_error, wo.created_at, wo.updated_at
)
SELECT c.id, c.room_id, c.room_event_id, c.target_agent_id, c.endpoint_id, c.event_type, c.payload, c.status, c.attempt_count, c.next_attempt_at, c.last_error, c.created_at, c.updated_at,
       ep.url, ep.secret_ciphertext, ep.key_id, ep.enabled
FROM claimed c
JOIN agent_webhook_endpoints ep ON ep.id = c.endpoint_id
ORDER BY c.id ASC`
	rows, err := exec.QueryContext(ctx, q, now, reclaimBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []repository.ClaimedWebhookDelivery
	for rows.Next() {
		item, scanErr := scanClaimedWebhookDelivery(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func normalizeErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return repository.ErrNotFound
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return repository.ErrConflict
	}
	return err
}
