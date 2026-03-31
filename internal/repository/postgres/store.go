package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

func (s *Store) CreateListing(ctx context.Context, in repository.CreateListingInput) (repository.Listing, error) {
	tagsJSON, err := json.Marshal(in.Tags)
	if err != nil {
		return repository.Listing{}, err
	}
	const q = `
INSERT INTO chat_listings (id, agent_id, topic, tags, max_turns, ttl_seconds, connected)
VALUES ($1, $2, $3, $4::jsonb, $5, $6, FALSE)
RETURNING id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at`
	return scanListing(s.db.QueryRowContext(ctx, q, in.ID, in.AgentID, in.Topic, tagsJSON, in.MaxTurns, in.TTLSeconds))
}

func (s *Store) GetListing(ctx context.Context, listingID string) (repository.Listing, error) {
	const q = `
SELECT id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at
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
SELECT id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at
FROM chat_listings
WHERE connected = FALSE`
	args := []any{}
	if strings.TrimSpace(query) != "" {
		base += ` AND (
  LOWER(topic) LIKE LOWER($1)
  OR EXISTS (
    SELECT 1
    FROM jsonb_array_elements_text(tags) AS t(value)
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

func (s *Store) CreateRoom(ctx context.Context, in repository.CreateRoomInput) (repository.Room, error) {
	const q = `
INSERT INTO rooms (id, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, human_code_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash`
	return scanRoom(
		s.db.QueryRowContext(ctx, q, in.ID, in.AgentAID, in.AgentBID, string(in.State), in.TurnIndex, in.MaxTurns, in.TTLAt, in.HumanCodeHash),
	)
}

func (s *Store) GetRoom(ctx context.Context, roomID string) (repository.Room, error) {
	const q = `
SELECT id, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash
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
	if in.TurnIndex != nil {
		current.TurnIndex = *in.TurnIndex
	}
	if in.ClosedAt != nil {
		current.ClosedAt = in.ClosedAt
	}
	if in.PurgedAt != nil {
		current.PurgedAt = in.PurgedAt
	}

	const q = `
UPDATE rooms
SET state = $2,
    turn_index = $3,
    closed_at = $4,
    purged_at = $5
WHERE id = $1
RETURNING id, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash`
	return scanRoom(s.db.QueryRowContext(
		ctx,
		q,
		current.ID,
		string(current.State),
		current.TurnIndex,
		current.ClosedAt,
		current.PurgedAt,
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
RETURNING room_id, context, version, updated_at, created_at`
	return scanRoomContext(s.db.QueryRowContext(ctx, q, in.RoomID, in.Context, in.Version))
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

func (s *Store) PurgeRoomContent(ctx context.Context, roomID string, purgedAt time.Time) error {
	const deleteMsgs = `DELETE FROM messages WHERE room_id = $1`
	if _, err := s.db.ExecContext(ctx, deleteMsgs, roomID); err != nil {
		return err
	}
	const clearContext = `DELETE FROM room_context_state WHERE room_id = $1`
	if _, err := s.db.ExecContext(ctx, clearContext, roomID); err != nil {
		return err
	}
	const clearViewers = `DELETE FROM room_viewers WHERE room_id = $1`
	if _, err := s.db.ExecContext(ctx, clearViewers, roomID); err != nil {
		return err
	}
	const updateRoom = `UPDATE rooms SET state = $2, purged_at = $3 WHERE id = $1`
	_, err := s.db.ExecContext(ctx, updateRoom, roomID, string(domain.RoomStatePurged), purgedAt)
	return err
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
SELECT r.id, r.agent_a_id, COALESCE(a.name, ''), r.agent_b_id, COALESCE(b.name, ''), r.state, r.turn_index, r.max_turns, r.ttl_at, r.created_at, r.closed_at, r.purged_at
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

func (s *txStore) GetListing(ctx context.Context, listingID string) (repository.Listing, error) {
	const q = `
SELECT id, agent_id, topic, tags, max_turns, ttl_seconds, connected, created_at
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
INSERT INTO rooms (id, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, human_code_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, agent_a_id, agent_b_id, state, turn_index, max_turns, ttl_at, created_at, closed_at, purged_at, human_code_hash`
	return scanRoom(
		s.tx.QueryRowContext(ctx, q, in.ID, in.AgentAID, in.AgentBID, string(in.State), in.TurnIndex, in.MaxTurns, in.TTLAt, in.HumanCodeHash),
	)
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
	err := row.Scan(&l.ID, &l.AgentID, &l.Topic, &raw, &l.MaxTurns, &l.TTLSeconds, &l.Connected, &l.CreatedAt)
	if err != nil {
		return repository.Listing{}, normalizeErr(err)
	}
	if len(raw) == 0 {
		l.Tags = []string{}
		return l, nil
	}
	if err := json.Unmarshal(raw, &l.Tags); err != nil {
		return repository.Listing{}, fmt.Errorf("decode tags: %w", err)
	}
	return l, nil
}

func scanListingRows(rows *sql.Rows) (repository.Listing, error) {
	return scanListing(rows)
}

func scanRoom(row interface{ Scan(dest ...any) error }) (repository.Room, error) {
	var r repository.Room
	var state string
	err := row.Scan(
		&r.ID,
		&r.AgentAID,
		&r.AgentBID,
		&state,
		&r.TurnIndex,
		&r.MaxTurns,
		&r.TTLAt,
		&r.CreatedAt,
		&r.ClosedAt,
		&r.PurgedAt,
		&r.HumanCodeHash,
	)
	if err != nil {
		return repository.Room{}, normalizeErr(err)
	}
	r.State = domain.RoomState(state)
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

func scanAuditEvent(row interface{ Scan(dest ...any) error }) (repository.AuditEvent, error) {
	var out repository.AuditEvent
	err := row.Scan(&out.ID, &out.RoomID, &out.Event, &out.Meta, &out.MessageCount, &out.CreatedAt)
	if err != nil {
		return repository.AuditEvent{}, normalizeErr(err)
	}
	return out, nil
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
