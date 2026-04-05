package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/febrian/areyouai/internal/domain"
)

type app struct {
	mu sync.Mutex

	agents          map[string]agent
	agentsByAPIHash map[string]string
	sessions        map[string]authSession
	listings        map[string]listing
	rooms           map[string]room
	messageWindows  map[string][]time.Time
	auditEvents     []auditEvent

	now                    func() time.Time
	viewerHeartbeatTimeout time.Duration
	closedRoomGraceDelay   time.Duration
	maxClosedRetention     time.Duration
}

type options struct {
	ViewerHeartbeatTimeout time.Duration
	ClosedRoomGraceDelay   time.Duration
	MaxClosedRetention     time.Duration
	AdminToken             string
	WebhookSecretKey       string
	WebhookSecretKeyset    string
	RoomDEKKey             string
	RoomDEKKeyset          string
}

type agent struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	APIKeyHash string
}

type authSession struct {
	AgentID   string
	ExpiresAt time.Time
}

type listing struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Topic     string    `json:"topic"`
	Tags      []string  `json:"tags"`
	MaxTurns  int       `json:"max_turns"`
	TTLSecond int       `json:"ttl_seconds"`
	CreatedAt time.Time `json:"created_at"`
	Connected bool      `json:"connected"`
	RoomID    string    `json:"-"`
}

type room struct {
	ID                 string           `json:"id"`
	Topic              string           `json:"topic,omitempty"`
	AgentAID           string           `json:"agent_a_id"`
	AgentBID           string           `json:"agent_b_id"`
	State              domain.RoomState `json:"state"`
	TurnIndex          int              `json:"turn_index"`
	MaxTurns           int              `json:"max_turns"`
	TTLAt              time.Time        `json:"ttl_at"`
	CreatedAt          time.Time        `json:"created_at"`
	ClosedAt           *time.Time       `json:"closed_at,omitempty"`
	PurgedAt           *time.Time       `json:"purged_at,omitempty"`
	HumanCodeHash      string
	HumanCodeExpiresAt *time.Time
	Joined             map[string]bool
	Viewers            map[string]viewerSession `json:"-"`
	Messages           []message
}

type message struct {
	ID         string    `json:"id"`
	RoomID     string    `json:"room_id"`
	SenderID   string    `json:"sender_id"`
	SenderName string    `json:"sender_name,omitempty"`
	Turn       int       `json:"turn"`
	Ciphertext string    `json:"ciphertext"`
	CreatedAt  time.Time `json:"created_at"`
}

type viewerSession struct {
	Token           string
	JoinedAt        time.Time
	LastHeartbeatAt time.Time
	LeftAt          *time.Time
}

type auditEvent struct {
	RoomID     string    `json:"room_id"`
	Event      string    `json:"event"`
	At         time.Time `json:"at"`
	Meta       string    `json:"meta"`
	MessageCnt int       `json:"message_count"`
}

func newApp(opts options) *app {
	a := &app{
		agents:          make(map[string]agent),
		agentsByAPIHash: make(map[string]string),
		sessions:        make(map[string]authSession),
		listings:        make(map[string]listing),
		rooms:           make(map[string]room),
		messageWindows:  make(map[string][]time.Time),
		auditEvents:     nil,
		now:             func() time.Time { return time.Now().UTC() },
	}
	if opts.ViewerHeartbeatTimeout > 0 {
		a.viewerHeartbeatTimeout = opts.ViewerHeartbeatTimeout
	} else {
		a.viewerHeartbeatTimeout = 45 * time.Second
	}
	if opts.ClosedRoomGraceDelay > 0 {
		a.closedRoomGraceDelay = opts.ClosedRoomGraceDelay
	} else {
		a.closedRoomGraceDelay = 2 * time.Minute
	}
	if opts.MaxClosedRetention > 0 {
		a.maxClosedRetention = opts.MaxClosedRetention
	} else {
		a.maxClosedRetention = 24 * time.Hour
	}
	return a
}

func newID(prefix string) string {
	return prefix + "_" + randomToken(12)
}

func randomToken(numBytes int) string {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashText(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:])
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func (a *app) purgeSweep() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.purgeSweepLocked(a.now())
}

func (a *app) purgeSweepLocked(now time.Time) {
	for id, rm := range a.rooms {
		switch rm.State {
		case domain.RoomStateOpen, domain.RoomStateActive:
			if now.After(rm.TTLAt) {
				closeTime := now
				rm.State = domain.RoomStateClosed
				rm.ClosedAt = &closeTime
				a.rooms[id] = rm
			}
		case domain.RoomStateClosed:
			if rm.ClosedAt == nil {
				closeTime := now
				rm.ClosedAt = &closeTime
			}

			activeViewers := activeViewerCount(rm, now, a.viewerHeartbeatTimeout)
			pastGrace := now.Sub(*rm.ClosedAt) >= a.closedRoomGraceDelay
			pastRetention := now.Sub(*rm.ClosedAt) >= a.maxClosedRetention
			if (activeViewers == 0 && pastGrace) || pastRetention {
				purgeTime := now
				msgCount := len(rm.Messages)
				rm.State = domain.RoomStatePurged
				rm.PurgedAt = &purgeTime
				rm.Messages = nil
				rm.Viewers = nil
				a.rooms[id] = rm
				a.auditEvents = append(a.auditEvents, auditEvent{
					RoomID:     id,
					Event:      "room_purged",
					At:         purgeTime,
					Meta:       "content hard-deleted",
					MessageCnt: msgCount,
				})
			}
		}
	}
}

func activeViewerCount(rm room, now time.Time, heartbeatTimeout time.Duration) int {
	if len(rm.Viewers) == 0 {
		return 0
	}

	active := 0
	for _, vw := range rm.Viewers {
		if vw.LeftAt != nil {
			continue
		}
		if now.Sub(vw.LastHeartbeatAt) > heartbeatTimeout {
			continue
		}
		active++
	}
	return active
}
