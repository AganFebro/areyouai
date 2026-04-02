package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/febrian/areyouai/internal/domain"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx TxStore) error) error
}

type Store interface {
	TxRunner

	CreateAgent(ctx context.Context, in CreateAgentInput) (Agent, error)
	FindAgentByAPIKeyHash(ctx context.Context, apiKeyHash string) (Agent, error)

	CreateSession(ctx context.Context, in CreateSessionInput) (Session, error)
	FindSession(ctx context.Context, token string) (Session, error)

	CreateListing(ctx context.Context, in CreateListingInput) (Listing, error)
	GetListing(ctx context.Context, listingID string) (Listing, error)
	MarkListingConnected(ctx context.Context, listingID string) error
	SearchListings(ctx context.Context, query string) ([]Listing, error)

	CreateRoom(ctx context.Context, in CreateRoomInput) (Room, error)
	GetRoom(ctx context.Context, roomID string) (Room, error)
	UpdateRoom(ctx context.Context, in UpdateRoomInput) (Room, error)

	AppendMessage(ctx context.Context, in AppendMessageInput) (Message, error)
	ListRoomMessages(ctx context.Context, roomID string) ([]Message, error)

	GetRoomContext(ctx context.Context, roomID string) (RoomContextState, error)
	UpsertRoomContext(ctx context.Context, in UpsertRoomContextInput) (RoomContextState, error)

	UpsertViewer(ctx context.Context, in UpsertViewerInput) (Viewer, error)
	GetViewer(ctx context.Context, viewerToken string) (Viewer, error)
	CountActiveViewers(ctx context.Context, roomID string, activeSince time.Time) (int, error)

	AppendAuditEvent(ctx context.Context, in AppendAuditEventInput) error
	AppendAPIRequestLog(ctx context.Context, in AppendAPIRequestLogInput) error
	AppendRoomEvent(ctx context.Context, in AppendRoomEventInput) (RoomEvent, error)
	GetRoomEvent(ctx context.Context, eventID int64) (RoomEvent, error)
	ListRoomEvents(ctx context.Context, in ListRoomEventsInput) ([]RoomEvent, error)
	CreateAgentWebhookEndpoint(ctx context.Context, in CreateAgentWebhookEndpointInput) (AgentWebhookEndpoint, error)
	ListAgentWebhookEndpoints(ctx context.Context, agentID string) ([]AgentWebhookEndpoint, error)
	DeleteAgentWebhookEndpoint(ctx context.Context, agentID, endpointID string) error
	CreateWebhookOutbox(ctx context.Context, in CreateWebhookOutboxInput) (WebhookOutboxItem, error)
	ClaimPendingWebhookDeliveries(ctx context.Context, now, reclaimBefore time.Time, limit int) ([]ClaimedWebhookDelivery, error)
	MarkWebhookOutboxDelivered(ctx context.Context, id int64) error
	MarkWebhookOutboxPendingRetry(ctx context.Context, id int64, nextAttemptAt time.Time, lastError string) error
	MarkWebhookOutboxDeadLetter(ctx context.Context, id int64, lastError string) error
	CreateRoomScopedToken(ctx context.Context, in CreateRoomScopedTokenInput) (RoomScopedToken, error)
	FindRoomScopedTokenByHash(ctx context.Context, tokenHash string) (RoomScopedToken, error)
	RevokeRoomScopedTokens(ctx context.Context, roomID, agentID string, revokedAt time.Time) error
	PurgeRoomContent(ctx context.Context, roomID string, purgedAt time.Time) error

	GetAdminOverview(ctx context.Context, now time.Time) (AdminOverview, error)
	ListAdminRooms(ctx context.Context, limit int) ([]AdminRoom, error)
	ListAuditEvents(ctx context.Context, limit int) ([]AuditEvent, error)
}

type TxStore interface {
	CreateListing(ctx context.Context, in CreateListingInput) (Listing, error)
	GetListing(ctx context.Context, listingID string) (Listing, error)
	MarkListingConnected(ctx context.Context, listingID string) error
	CreateRoom(ctx context.Context, in CreateRoomInput) (Room, error)
	GetRoom(ctx context.Context, roomID string) (Room, error)
	UpdateRoom(ctx context.Context, in UpdateRoomInput) (Room, error)
	AppendMessage(ctx context.Context, in AppendMessageInput) (Message, error)
	PurgeRoomContent(ctx context.Context, roomID string, purgedAt time.Time) error
	AppendRoomEvent(ctx context.Context, in AppendRoomEventInput) (RoomEvent, error)
	ListAgentWebhookEndpoints(ctx context.Context, agentID string) ([]AgentWebhookEndpoint, error)
	CreateWebhookOutbox(ctx context.Context, in CreateWebhookOutboxInput) (WebhookOutboxItem, error)
	CreateRoomScopedToken(ctx context.Context, in CreateRoomScopedTokenInput) (RoomScopedToken, error)
	RevokeRoomScopedTokens(ctx context.Context, roomID, agentID string, revokedAt time.Time) error
}

type Agent struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	APIKeyHash string    `json:"api_key_hash,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Session struct {
	Token     string     `json:"token"`
	AgentID   string     `json:"agent_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type Listing struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	Topic      string    `json:"topic"`
	Tags       []string  `json:"tags"`
	MaxTurns   int       `json:"max_turns"`
	TTLSeconds int       `json:"ttl_seconds"`
	Connected  bool      `json:"connected"`
	CreatedAt  time.Time `json:"created_at"`
	RoomID     string    `json:"-"`
}

type Room struct {
	ID            string           `json:"id"`
	AgentAID      string           `json:"agent_a_id"`
	AgentBID      string           `json:"agent_b_id"`
	State         domain.RoomState `json:"state"`
	TurnIndex     int              `json:"turn_index"`
	MaxTurns      int              `json:"max_turns"`
	TTLAt         time.Time        `json:"ttl_at"`
	CreatedAt     time.Time        `json:"created_at"`
	ClosedAt      *time.Time       `json:"closed_at,omitempty"`
	PurgedAt      *time.Time       `json:"purged_at,omitempty"`
	HumanCodeHash string           `json:"human_code_hash,omitempty"`
}

type Message struct {
	ID         string    `json:"id"`
	RoomID     string    `json:"room_id"`
	SenderID   string    `json:"sender_id"`
	SenderName string    `json:"sender_name,omitempty"`
	Turn       int       `json:"turn"`
	Ciphertext string    `json:"ciphertext"`
	CreatedAt  time.Time `json:"created_at"`
}

type Viewer struct {
	ID              string     `json:"id"`
	RoomID          string     `json:"room_id"`
	ViewerToken     string     `json:"viewer_token"`
	JoinedAt        time.Time  `json:"joined_at"`
	LastHeartbeatAt time.Time  `json:"last_heartbeat_at"`
	LeftAt          *time.Time `json:"left_at,omitempty"`
}

type RoomContextState struct {
	RoomID    string          `json:"room_id"`
	Context   json.RawMessage `json:"context"`
	Version   int             `json:"version"`
	UpdatedAt time.Time       `json:"updated_at"`
	CreatedAt time.Time       `json:"created_at"`
}

type AdminOverview struct {
	AgentsTotal    int `json:"agents_total"`
	SessionsActive int `json:"sessions_active"`
	RoomsOpen      int `json:"rooms_open"`
	RoomsActive    int `json:"rooms_active"`
	RoomsClosed    int `json:"rooms_closed"`
	RoomsPurged    int `json:"rooms_purged"`
	MessagesTotal  int `json:"messages_total"`
}

type AdminRoom struct {
	ID         string           `json:"id"`
	AgentAID   string           `json:"agent_a_id"`
	AgentAName string           `json:"agent_a_name"`
	AgentBID   string           `json:"agent_b_id"`
	AgentBName string           `json:"agent_b_name"`
	State      domain.RoomState `json:"state"`
	TurnIndex  int              `json:"turn_index"`
	MaxTurns   int              `json:"max_turns"`
	TTLAt      time.Time        `json:"ttl_at"`
	CreatedAt  time.Time        `json:"created_at"`
	ClosedAt   *time.Time       `json:"closed_at,omitempty"`
	PurgedAt   *time.Time       `json:"purged_at,omitempty"`
}

type AuditEvent struct {
	ID           int64     `json:"id"`
	RoomID       string    `json:"room_id"`
	Event        string    `json:"event"`
	Meta         string    `json:"meta"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
}

type APIRequestLog struct {
	ID           int64     `json:"id"`
	RequestID    string    `json:"request_id"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Query        string    `json:"query"`
	StatusCode   int       `json:"status_code"`
	DurationMS   int       `json:"duration_ms"`
	RemoteIP     string    `json:"remote_ip"`
	UserAgent    string    `json:"user_agent"`
	BytesWritten int64     `json:"bytes_written"`
	AuthPresent  bool      `json:"auth_present"`
	CreatedAt    time.Time `json:"created_at"`
}

type RoomEvent struct {
	ID         int64     `json:"id"`
	RoomID     string    `json:"room_id"`
	EventType  string    `json:"event_type"`
	MessageID  *string   `json:"message_id,omitempty"`
	Turn       *int      `json:"turn,omitempty"`
	SenderID   *string   `json:"sender_id,omitempty"`
	Ciphertext *string   `json:"ciphertext,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type AgentWebhookEndpoint struct {
	ID               string    `json:"id"`
	AgentID          string    `json:"agent_id"`
	URL              string    `json:"url"`
	SecretCiphertext string    `json:"-"`
	KeyID            string    `json:"key_id"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type WebhookOutboxItem struct {
	ID            int64           `json:"id"`
	RoomID        string          `json:"room_id"`
	RoomEventID   int64           `json:"room_event_id"`
	TargetAgentID string          `json:"target_agent_id"`
	EndpointID    string          `json:"endpoint_id"`
	EventType     string          `json:"event_type"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	AttemptCount  int             `json:"attempt_count"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type ClaimedWebhookDelivery struct {
	WebhookOutboxItem
	EndpointURL              string `json:"endpoint_url"`
	EndpointSecretCiphertext string `json:"-"`
	EndpointKeyID            string `json:"endpoint_key_id"`
	EndpointEnabled          bool   `json:"endpoint_enabled"`
}

type RoomScopedToken struct {
	ID        string     `json:"id"`
	RoomID    string     `json:"room_id"`
	AgentID   string     `json:"agent_id"`
	TokenHash string     `json:"-"`
	Scope     string     `json:"scope"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type CreateAgentInput struct {
	ID         string
	Name       string
	APIKeyHash string
}

type CreateSessionInput struct {
	Token     string
	AgentID   string
	ExpiresAt *time.Time
}

type CreateListingInput struct {
	ID         string
	AgentID    string
	Topic      string
	Tags       []string
	MaxTurns   int
	TTLSeconds int
	RoomID     string
}

type CreateRoomInput struct {
	ID            string
	AgentAID      string
	AgentBID      string
	State         domain.RoomState
	TurnIndex     int
	MaxTurns      int
	TTLAt         time.Time
	HumanCodeHash string
}

type UpdateRoomInput struct {
	ID        string
	AgentBID  *string
	State     *domain.RoomState
	TurnIndex *int
	ClosedAt  *time.Time
	PurgedAt  *time.Time
}

type AppendMessageInput struct {
	ID         string
	RoomID     string
	SenderID   string
	Turn       int
	Ciphertext string
}

type UpsertViewerInput struct {
	ID              string
	RoomID          string
	ViewerToken     string
	JoinedAt        time.Time
	LastHeartbeatAt time.Time
	LeftAt          *time.Time
}

type AppendAuditEventInput struct {
	RoomID       string
	Event        string
	Meta         string
	MessageCount int
}

type AppendAPIRequestLogInput struct {
	RequestID    string
	Method       string
	Path         string
	Query        string
	StatusCode   int
	DurationMS   int
	RemoteIP     string
	UserAgent    string
	BytesWritten int64
	AuthPresent  bool
}

type AppendRoomEventInput struct {
	RoomID     string
	EventType  string
	MessageID  *string
	Turn       *int
	SenderID   *string
	Ciphertext *string
}

type ListRoomEventsInput struct {
	RoomID  string
	SinceID int64
	Limit   int
}

type UpsertRoomContextInput struct {
	RoomID  string
	Context json.RawMessage
	Version int
}

type CreateAgentWebhookEndpointInput struct {
	ID               string
	AgentID          string
	URL              string
	SecretCiphertext string
	KeyID            string
	Enabled          bool
}

type CreateWebhookOutboxInput struct {
	RoomID        string
	RoomEventID   int64
	TargetAgentID string
	EndpointID    string
	EventType     string
	Payload       json.RawMessage
	Status        string
	AttemptCount  int
	NextAttemptAt time.Time
	LastError     string
}

type CreateRoomScopedTokenInput struct {
	ID        string
	RoomID    string
	AgentID   string
	TokenHash string
	Scope     string
	ExpiresAt time.Time
}
