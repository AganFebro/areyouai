package a2a

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/febrian/areyouai/internal/domain"
	"github.com/febrian/areyouai/internal/repository"
	"github.com/febrian/areyouai/internal/security"
	"github.com/febrian/areyouai/internal/service/promptbuilder"
)

const (
	maxMessagesPerMinuteAgent = 30
	maxMessagesPerMinuteRoom  = 60
	policyViolationWindow     = 5 * time.Minute
	policyBlockDuration       = 15 * time.Minute
	maxPolicyViolationsWindow = 3
	maxRecentMemoryEntries    = 6
	maxRoomEventHistoryLimit  = 200
	sessionTTL                = 14 * 24 * time.Hour
)

var (
	ErrBadRequest    = errors.New("bad request")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbidden     = errors.New("forbidden")
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrGone          = errors.New("gone")
	ErrRateLimit     = errors.New("rate limit")
	ErrPolicyBlocked = errors.New("policy blocked")
)

type Service struct {
	store repository.Store
	pb    *promptbuilder.Builder
	emit  func(repository.RoomEvent)

	mu             sync.Mutex
	joined         map[string]map[string]bool
	messageWindows map[string][]time.Time
	roomWindows    map[string][]time.Time
	policyWindows  map[string][]time.Time
	blockedAgents  map[string]time.Time

	now                    func() time.Time
	viewerHeartbeatTimeout time.Duration
	closedRoomGraceDelay   time.Duration
	maxClosedRetention     time.Duration
}

type Options struct {
	ViewerHeartbeatTimeout time.Duration
	ClosedRoomGraceDelay   time.Duration
	MaxClosedRetention     time.Duration
	RoomEventPublisher     func(repository.RoomEvent)
}

type RegisterResult struct {
	AgentID string
	APIKey  string
}

type LoginResult struct {
	SessionToken string
}

type ConnectResult struct {
	RoomID    string
	HumanCode string
	AgentAID  string
	AgentBID  string
	RoomState domain.RoomState
	ListingID string
	NextTurnA string
}

func New(store repository.Store, opts Options) *Service {
	pb, err := promptbuilder.NewDefaultBuilder()
	if err != nil {
		panic(fmt.Errorf("promptbuilder init failed: %w", err))
	}

	s := &Service{
		store:                  store,
		pb:                     pb,
		emit:                   func(repository.RoomEvent) {},
		joined:                 make(map[string]map[string]bool),
		messageWindows:         make(map[string][]time.Time),
		roomWindows:            make(map[string][]time.Time),
		policyWindows:          make(map[string][]time.Time),
		blockedAgents:          make(map[string]time.Time),
		now:                    func() time.Time { return time.Now().UTC() },
		viewerHeartbeatTimeout: 45 * time.Second,
		closedRoomGraceDelay:   2 * time.Minute,
		maxClosedRetention:     24 * time.Hour,
	}
	if opts.ViewerHeartbeatTimeout > 0 {
		s.viewerHeartbeatTimeout = opts.ViewerHeartbeatTimeout
	}
	if opts.ClosedRoomGraceDelay > 0 {
		s.closedRoomGraceDelay = opts.ClosedRoomGraceDelay
	}
	if opts.MaxClosedRetention > 0 {
		s.maxClosedRetention = opts.MaxClosedRetention
	}
	if opts.RoomEventPublisher != nil {
		s.emit = opts.RoomEventPublisher
	}
	return s
}

func (s *Service) RegisterAgent(ctx context.Context, name string) (RegisterResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return RegisterResult{}, ErrBadRequest
	}

	apiKey := "ak_" + randomToken(24)
	agentID := newID("agt")
	_, err := s.store.CreateAgent(ctx, repository.CreateAgentInput{
		ID:         agentID,
		Name:       name,
		APIKeyHash: hashText(apiKey),
	})
	if err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{AgentID: agentID, APIKey: apiKey}, nil
}

func (s *Service) Login(ctx context.Context, apiKey string) (LoginResult, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return LoginResult{}, ErrBadRequest
	}

	agent, err := s.store.FindAgentByAPIKeyHash(ctx, hashText(apiKey))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return LoginResult{}, ErrUnauthorized
		}
		return LoginResult{}, err
	}

	token := "as_" + randomToken(24)
	expiresAt := s.now().Add(sessionTTL)
	_, err = s.store.CreateSession(ctx, repository.CreateSessionInput{
		Token:     token,
		AgentID:   agent.ID,
		ExpiresAt: &expiresAt,
	})
	if err != nil {
		return LoginResult{}, err
	}

	return LoginResult{SessionToken: token}, nil
}

func (s *Service) AuthAgentID(ctx context.Context, bearerToken string) (string, error) {
	token := strings.TrimSpace(bearerToken)
	if token == "" {
		return "", ErrUnauthorized
	}
	session, err := s.store.FindSession(ctx, token)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", ErrUnauthorized
		}
		return "", err
	}
	if session.ExpiresAt == nil || !session.ExpiresAt.After(s.now()) {
		return "", ErrUnauthorized
	}
	return session.AgentID, nil
}

func (s *Service) CreateListing(ctx context.Context, agentID, topic string, tags []string, maxTurns, ttlSeconds int) (repository.Listing, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return repository.Listing{}, ErrBadRequest
	}
	if maxTurns <= 0 {
		maxTurns = 20
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 900
	}
	return s.store.CreateListing(ctx, repository.CreateListingInput{
		ID:         newID("lst"),
		AgentID:    agentID,
		Topic:      topic,
		Tags:       tags,
		MaxTurns:   maxTurns,
		TTLSeconds: ttlSeconds,
	})
}

func (s *Service) SearchListings(ctx context.Context, query string) ([]repository.Listing, error) {
	return s.store.SearchListings(ctx, strings.TrimSpace(strings.ToLower(query)))
}

func (s *Service) ConnectListing(ctx context.Context, agentID, listingID string) (ConnectResult, error) {
	humanCode := "hc_" + randomToken(18)
	now := s.now()
	res := ConnectResult{}

	err := s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
		l, err := tx.GetListing(ctx, listingID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ErrNotFound
			}
			return err
		}
		if l.AgentID == agentID {
			return ErrForbidden
		}
		if l.Connected {
			return ErrConflict
		}
		if err := tx.MarkListingConnected(ctx, l.ID); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ErrConflict
			}
			return err
		}

		rm, err := tx.CreateRoom(ctx, repository.CreateRoomInput{
			ID:            newID("room"),
			AgentAID:      l.AgentID,
			AgentBID:      agentID,
			State:         domain.RoomStateOpen,
			TurnIndex:     0,
			MaxTurns:      l.MaxTurns,
			TTLAt:         now.Add(time.Duration(l.TTLSeconds) * time.Second),
			HumanCodeHash: hashText(humanCode),
		})
		if err != nil {
			return err
		}

		res = ConnectResult{
			RoomID:    rm.ID,
			HumanCode: humanCode,
			AgentAID:  rm.AgentAID,
			AgentBID:  rm.AgentBID,
			RoomState: rm.State,
			ListingID: l.ID,
			NextTurnA: rm.AgentAID,
		}
		return nil
	})
	if err != nil {
		return ConnectResult{}, err
	}
	createdRoom, err := s.store.GetRoom(ctx, res.RoomID)
	if err != nil {
		return ConnectResult{}, err
	}
	if err := s.upsertRoomContext(ctx, createdRoom, ""); err != nil {
		return ConnectResult{}, err
	}
	return res, nil
}

func (s *Service) JoinRoom(ctx context.Context, agentID, roomID string) (domain.RoomState, map[string]bool, error) {
	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", nil, ErrNotFound
		}
		return "", nil, err
	}
	rm, err = s.reconcileRoom(ctx, rm)
	if err != nil {
		return "", nil, err
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		return "", nil, ErrForbidden
	}
	if rm.State == domain.RoomStateClosed || rm.State == domain.RoomStatePurged {
		return "", nil, ErrGone
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.joined[roomID] == nil {
		s.joined[roomID] = map[string]bool{}
	}
	s.joined[roomID][agentID] = true
	joined := map[string]bool{
		rm.AgentAID: s.joined[roomID][rm.AgentAID],
		rm.AgentBID: s.joined[roomID][rm.AgentBID],
	}
	if joined[rm.AgentAID] && joined[rm.AgentBID] && rm.State == domain.RoomStateOpen {
		next := domain.RoomStateActive
		var updatedRoom repository.Room
		var emitted []repository.RoomEvent
		err := s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
			room, txErr := tx.UpdateRoom(ctx, repository.UpdateRoomInput{
				ID:    rm.ID,
				State: &next,
			})
			if txErr != nil {
				return txErr
			}
			ev, txErr := tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
				RoomID:    rm.ID,
				EventType: "room.state_changed",
				SenderID:  &agentID,
			})
			if txErr != nil {
				return txErr
			}
			emitted = append(emitted, ev)
			updatedRoom = room
			return nil
		})
		if err != nil {
			return "", nil, err
		}
		s.publishRoomEvents(emitted)
		if err := s.upsertRoomContext(ctx, updatedRoom, ""); err != nil {
			return "", nil, err
		}
		return next, joined, nil
	}
	if err := s.upsertRoomContext(ctx, rm, ""); err != nil {
		return "", nil, err
	}
	return rm.State, joined, nil
}

type SendMessageResult struct {
	Message    repository.Message
	RoomState  domain.RoomState
	NextTurn   int
	BundleHash string
}

type PromptBundleResult struct {
	BundleHash      string
	SystemCoreHash  string
	GlobalRulesHash string
	AgentRulesHash  string
	Prompt          string
}

type roomContextPayload struct {
	RoomID       string              `json:"room_id"`
	AgentAID     string              `json:"agent_a_id"`
	AgentBID     string              `json:"agent_b_id"`
	LastActorID  string              `json:"last_actor_id,omitempty"`
	State        string              `json:"state"`
	TurnIndex    int                 `json:"turn_index"`
	MaxTurns     int                 `json:"max_turns"`
	TTLAt        string              `json:"ttl_at"`
	ClosedAt     *string             `json:"closed_at,omitempty"`
	RecentMemory []recentMemoryEntry `json:"recent_memory"`
}

type recentMemoryEntry struct {
	Turn     int    `json:"turn"`
	SenderID string `json:"sender_id"`
}

func (s *Service) SendMessage(ctx context.Context, agentID, roomID string, expectedTurn int, ciphertext, providedBundleHash string) (SendMessageResult, error) {
	ciphertext = strings.TrimSpace(ciphertext)
	if ciphertext == "" {
		return SendMessageResult{}, ErrBadRequest
	}
	providedBundleHash = strings.TrimSpace(providedBundleHash)
	if providedBundleHash == "" {
		return SendMessageResult{}, ErrBadRequest
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return SendMessageResult{}, ErrNotFound
		}
		return SendMessageResult{}, err
	}
	rm, err = s.reconcileRoomLocked(ctx, rm)
	if err != nil {
		return SendMessageResult{}, err
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		return SendMessageResult{}, ErrForbidden
	}
	if rm.State == domain.RoomStateClosed || rm.State == domain.RoomStatePurged {
		return SendMessageResult{}, ErrGone
	}
	if s.now().After(rm.TTLAt) {
		return SendMessageResult{}, ErrGone
	}
	now := s.now()
	if until, blocked := s.blockedUntilLocked(agentID, now); blocked {
		s.appendAuditEventBestEffort(ctx, roomID, "message_policy_blocked", map[string]any{
			"room_id":       roomID,
			"agent_id":      agentID,
			"code":          "agent_temporarily_blocked",
			"reason":        "too many policy violations",
			"blocked_until": until.Format(time.RFC3339),
		}, 0)
		return SendMessageResult{}, ErrPolicyBlocked
	}
	if !s.allowAgentMessageLocked(agentID, now) {
		s.appendAuditEventBestEffort(ctx, roomID, "message_rate_limited", map[string]any{
			"room_id":  roomID,
			"agent_id": agentID,
			"scope":    "agent",
		}, 0)
		return SendMessageResult{}, ErrRateLimit
	}
	if !s.allowRoomMessageLocked(roomID, now) {
		s.appendAuditEventBestEffort(ctx, roomID, "message_rate_limited", map[string]any{
			"room_id":  roomID,
			"agent_id": agentID,
			"scope":    "room",
		}, 0)
		return SendMessageResult{}, ErrRateLimit
	}
	if expectedTurn != rm.TurnIndex {
		return SendMessageResult{}, ErrConflict
	}

	expectedSender := rm.AgentAID
	if rm.TurnIndex%2 == 1 {
		expectedSender = rm.AgentBID
	}
	if expectedSender != agentID {
		return SendMessageResult{}, ErrConflict
	}

	decision := security.EvaluateMessageForPersist(ciphertext)
	if !decision.Allowed {
		violations := s.recordPolicyViolationLocked(agentID, now)
		s.appendAuditEventBestEffort(ctx, roomID, "message_policy_blocked", map[string]any{
			"room_id":          roomID,
			"agent_id":         agentID,
			"code":             decision.Code,
			"reason":           decision.Reason,
			"violation_count":  violations,
			"blocked_temporal": violations >= maxPolicyViolationsWindow,
		}, 0)
		return SendMessageResult{}, ErrPolicyBlocked
	}

	bundle, recentCount, err := s.buildBundleForRoom(ctx, rm, agentID)
	if err != nil {
		return SendMessageResult{}, err
	}
	if !strings.EqualFold(bundle.BundleHash, providedBundleHash) {
		s.appendAuditEventBestEffort(ctx, roomID, "bundle_hash_mismatch", map[string]any{
			"room_id":       roomID,
			"agent_id":      agentID,
			"expected_hash": bundle.BundleHash,
			"provided_hash": providedBundleHash,
			"turn_index":    rm.TurnIndex,
		}, recentCount)
		return SendMessageResult{}, ErrConflict
	}

	var msg repository.Message
	nextTurn := rm.TurnIndex + 1
	nextState := rm.State
	var updatedRoom repository.Room
	var emitted []repository.RoomEvent
	err = s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
		var txErr error
		msg, txErr = tx.AppendMessage(ctx, repository.AppendMessageInput{
			ID:         newID("msg"),
			RoomID:     roomID,
			SenderID:   agentID,
			Turn:       rm.TurnIndex,
			Ciphertext: ciphertext,
		})
		if txErr != nil {
			if errors.Is(txErr, repository.ErrConflict) {
				return ErrConflict
			}
			return txErr
		}

		update := repository.UpdateRoomInput{
			ID:        rm.ID,
			TurnIndex: &nextTurn,
		}
		if nextTurn >= rm.MaxTurns {
			now := s.now()
			closed := domain.RoomStateClosed
			update.State = &closed
			update.ClosedAt = &now
			nextState = closed
		}
		updatedRoom, txErr = tx.UpdateRoom(ctx, update)
		if txErr != nil {
			return txErr
		}

		ev, txErr := tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
			RoomID:     rm.ID,
			EventType:  "message.created",
			MessageID:  &msg.ID,
			Turn:       &msg.Turn,
			SenderID:   &agentID,
			Ciphertext: &msg.Ciphertext,
		})
		if txErr != nil {
			return txErr
		}
		emitted = append(emitted, ev)

		if nextState == domain.RoomStateClosed {
			ev, txErr = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
				RoomID:    rm.ID,
				EventType: "room.state_changed",
				SenderID:  &agentID,
			})
			if txErr != nil {
				return txErr
			}
			emitted = append(emitted, ev)
			ev, txErr = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
				RoomID:    rm.ID,
				EventType: "room.closed",
				SenderID:  &agentID,
			})
			if txErr != nil {
				return txErr
			}
			emitted = append(emitted, ev)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return SendMessageResult{}, ErrConflict
		}
		return SendMessageResult{}, err
	}
	s.publishRoomEvents(emitted)
	if err := s.upsertRoomContext(ctx, updatedRoom, agentID); err != nil {
		s.appendAuditEventBestEffort(ctx, roomID, "room_context_sync_failed", map[string]any{
			"room_id":    roomID,
			"agent_id":   agentID,
			"turn_index": msg.Turn,
			"reason":     err.Error(),
		}, 0)
	}

	meta, _ := json.Marshal(map[string]any{
		"bundle_hash":       bundle.BundleHash,
		"system_core_hash":  bundle.SystemCoreHash,
		"global_rules_hash": bundle.GlobalRulesHash,
		"agent_rules_hash":  bundle.AgentRulesHash,
		"room_id":           roomID,
		"agent_id":          agentID,
		"turn_index":        msg.Turn,
	})
	_ = s.store.AppendAuditEvent(ctx, repository.AppendAuditEventInput{
		RoomID:       roomID,
		Event:        "prompt_bundle_generated",
		Meta:         string(meta),
		MessageCount: recentCount,
	})
	s.appendAuditEventBestEffort(ctx, roomID, "message_persisted", map[string]any{
		"room_id":      roomID,
		"agent_id":     agentID,
		"turn_index":   msg.Turn,
		"next_turn":    nextTurn,
		"bundle_hash":  bundle.BundleHash,
		"message_id":   msg.ID,
		"room_state":   string(nextState),
		"audit_source": "service_send_message",
	}, recentCount)

	return SendMessageResult{
		Message:    msg,
		RoomState:  nextState,
		NextTurn:   nextTurn,
		BundleHash: bundle.BundleHash,
	}, nil
}

func (s *Service) GetPromptBundle(ctx context.Context, agentID, roomID string) (PromptBundleResult, error) {
	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return PromptBundleResult{}, ErrNotFound
		}
		return PromptBundleResult{}, err
	}
	rm, err = s.reconcileRoom(ctx, rm)
	if err != nil {
		return PromptBundleResult{}, err
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		return PromptBundleResult{}, ErrForbidden
	}

	bundle, _, err := s.buildBundleForRoom(ctx, rm, agentID)
	if err != nil {
		return PromptBundleResult{}, err
	}
	return PromptBundleResult{
		BundleHash:      bundle.BundleHash,
		SystemCoreHash:  bundle.SystemCoreHash,
		GlobalRulesHash: bundle.GlobalRulesHash,
		AgentRulesHash:  bundle.AgentRulesHash,
		Prompt:          bundle.Prompt,
	}, nil
}

func (s *Service) buildBundleForRoom(ctx context.Context, rm repository.Room, agentID string) (promptbuilder.Bundle, int, error) {
	payload := s.roomContextFromRoom(rm, "")
	roomContextState, err := s.store.GetRoomContext(ctx, rm.ID)
	if err == nil {
		var persisted roomContextPayload
		if unmarshalErr := json.Unmarshal(roomContextState.Context, &persisted); unmarshalErr == nil {
			// Live room state is authoritative; only carry forward stable, optional
			// continuity metadata from persisted context.
			if persisted.LastActorID == rm.AgentAID || persisted.LastActorID == rm.AgentBID {
				payload.LastActorID = persisted.LastActorID
			}
		}
	} else if !errors.Is(err, repository.ErrNotFound) {
		return promptbuilder.Bundle{}, 0, err
	}

	recent, listErr := s.store.ListRoomMessages(ctx, rm.ID)
	if listErr != nil {
		return promptbuilder.Bundle{}, 0, listErr
	}
	recentForBundle := make([]promptbuilder.RecentMessage, 0, len(recent))
	for _, m := range recent {
		recentForBundle = append(recentForBundle, promptbuilder.RecentMessage{
			Turn:       m.Turn,
			SenderID:   m.SenderID,
			Ciphertext: m.Ciphertext,
		})
	}

	taskContext := s.formatTaskContext(payload, agentID)
	return s.pb.Build(promptbuilder.BuildInput{
		TaskContext:    taskContext,
		RecentMessages: recentForBundle,
	}), len(recentForBundle), nil
}

type RoomStateResult struct {
	Room          repository.Room
	ActiveViewers int
}

type RoomEventHistoryResult struct {
	Items     []repository.RoomEvent
	NextSince int64
}

type AdminOverviewResult struct {
	Overview repository.AdminOverview
}

func (s *Service) AdminOverview(ctx context.Context) (AdminOverviewResult, error) {
	out, err := s.store.GetAdminOverview(ctx, s.now())
	if err != nil {
		return AdminOverviewResult{}, err
	}
	return AdminOverviewResult{Overview: out}, nil
}

func (s *Service) AdminRooms(ctx context.Context, limit int) ([]repository.AdminRoom, error) {
	return s.store.ListAdminRooms(ctx, limit)
}

func (s *Service) AdminAudit(ctx context.Context, limit int) ([]repository.AuditEvent, error) {
	return s.store.ListAuditEvents(ctx, limit)
}

func (s *Service) ListRoomEventHistory(ctx context.Context, agentID, roomID string, sinceID int64, limit int) (RoomEventHistoryResult, error) {
	if sinceID < 0 {
		return RoomEventHistoryResult{}, ErrBadRequest
	}
	if limit <= 0 || limit > maxRoomEventHistoryLimit {
		limit = maxRoomEventHistoryLimit
	}

	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return RoomEventHistoryResult{}, ErrNotFound
		}
		return RoomEventHistoryResult{}, err
	}
	rm, err = s.reconcileRoom(ctx, rm)
	if err != nil {
		return RoomEventHistoryResult{}, err
	}
	if rm.State == domain.RoomStatePurged {
		return RoomEventHistoryResult{}, ErrGone
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		return RoomEventHistoryResult{}, ErrForbidden
	}

	s.mu.Lock()
	joined := s.joined[roomID] != nil && s.joined[roomID][agentID]
	s.mu.Unlock()
	if !joined {
		return RoomEventHistoryResult{}, ErrForbidden
	}

	if sinceID > 0 {
		sinceEvent, getErr := s.store.GetRoomEvent(ctx, sinceID)
		if getErr != nil {
			if errors.Is(getErr, repository.ErrNotFound) {
				return RoomEventHistoryResult{}, ErrBadRequest
			}
			return RoomEventHistoryResult{}, getErr
		}
		if sinceEvent.RoomID != roomID {
			return RoomEventHistoryResult{}, ErrBadRequest
		}
	}

	items, err := s.store.ListRoomEvents(ctx, repository.ListRoomEventsInput{
		RoomID:  roomID,
		SinceID: sinceID,
		Limit:   limit,
	})
	if err != nil {
		return RoomEventHistoryResult{}, err
	}

	nextSince := sinceID
	if len(items) > 0 {
		nextSince = items[len(items)-1].ID
	}
	return RoomEventHistoryResult{
		Items:     items,
		NextSince: nextSince,
	}, nil
}

func (s *Service) GetRoomState(ctx context.Context, agentID, roomID string) (RoomStateResult, error) {
	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return RoomStateResult{}, ErrNotFound
		}
		return RoomStateResult{}, err
	}
	rm, err = s.reconcileRoom(ctx, rm)
	if err != nil {
		return RoomStateResult{}, err
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		return RoomStateResult{}, ErrForbidden
	}
	activeSince := s.now().Add(-s.viewerHeartbeatTimeout)
	count, err := s.store.CountActiveViewers(ctx, roomID, activeSince)
	if err != nil {
		return RoomStateResult{}, err
	}
	return RoomStateResult{Room: rm, ActiveViewers: count}, nil
}

func (s *Service) CloseRoom(ctx context.Context, agentID, roomID string) (repository.Room, error) {
	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return repository.Room{}, ErrNotFound
		}
		return repository.Room{}, err
	}
	rm, err = s.reconcileRoom(ctx, rm)
	if err != nil {
		return repository.Room{}, err
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		return repository.Room{}, ErrForbidden
	}
	if rm.State == domain.RoomStatePurged {
		return repository.Room{}, ErrGone
	}
	if rm.State == domain.RoomStateClosed {
		// Idempotent close: already terminal, no new events.
		return rm, nil
	}

	now := s.now()
	closed := domain.RoomStateClosed
	var updatedRoom repository.Room
	var emitted []repository.RoomEvent
	err = s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
		room, txErr := tx.UpdateRoom(ctx, repository.UpdateRoomInput{
			ID:       roomID,
			State:    &closed,
			ClosedAt: &now,
		})
		if txErr != nil {
			return txErr
		}
		ev, txErr := tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
			RoomID:    roomID,
			EventType: "room.state_changed",
			SenderID:  &agentID,
		})
		if txErr != nil {
			return txErr
		}
		emitted = append(emitted, ev)
		ev, txErr = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
			RoomID:    roomID,
			EventType: "room.closed",
			SenderID:  &agentID,
		})
		if txErr != nil {
			return txErr
		}
		emitted = append(emitted, ev)
		updatedRoom = room
		return nil
	})
	if err != nil {
		return repository.Room{}, err
	}
	s.publishRoomEvents(emitted)
	if err := s.upsertRoomContext(ctx, updatedRoom, agentID); err != nil {
		return repository.Room{}, err
	}
	return updatedRoom, nil
}

type TranscriptResult struct {
	Room     repository.Room
	Messages []repository.Message
}

func (s *Service) Transcript(ctx context.Context, roomID, humanCode string) (TranscriptResult, error) {
	humanCode = strings.TrimSpace(humanCode)
	if humanCode == "" {
		return TranscriptResult{}, ErrForbidden
	}

	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return TranscriptResult{}, ErrNotFound
		}
		return TranscriptResult{}, err
	}
	rm, err = s.reconcileRoom(ctx, rm)
	if err != nil {
		return TranscriptResult{}, err
	}
	if rm.State == domain.RoomStatePurged {
		return TranscriptResult{}, ErrGone
	}
	if subtle.ConstantTimeCompare([]byte(hashText(humanCode)), []byte(rm.HumanCodeHash)) != 1 {
		return TranscriptResult{}, ErrForbidden
	}

	msgs, err := s.store.ListRoomMessages(ctx, roomID)
	if err != nil {
		return TranscriptResult{}, err
	}
	return TranscriptResult{Room: rm, Messages: msgs}, nil
}

type ViewerResult struct {
	ViewerToken   string
	ActiveViewers int
}

func (s *Service) ViewerJoin(ctx context.Context, roomID, humanCode string) (ViewerResult, error) {
	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ViewerResult{}, ErrNotFound
		}
		return ViewerResult{}, err
	}
	rm, err = s.reconcileRoom(ctx, rm)
	if err != nil {
		return ViewerResult{}, err
	}
	if rm.State == domain.RoomStatePurged {
		return ViewerResult{}, ErrGone
	}
	if subtle.ConstantTimeCompare([]byte(hashText(strings.TrimSpace(humanCode))), []byte(rm.HumanCodeHash)) != 1 {
		return ViewerResult{}, ErrForbidden
	}

	now := s.now()
	token := "hv_" + randomToken(18)
	_, err = s.store.UpsertViewer(ctx, repository.UpsertViewerInput{
		ID:              newID("rvw"),
		RoomID:          roomID,
		ViewerToken:     token,
		JoinedAt:        now,
		LastHeartbeatAt: now,
	})
	if err != nil {
		return ViewerResult{}, err
	}
	count, err := s.store.CountActiveViewers(ctx, roomID, s.now().Add(-s.viewerHeartbeatTimeout))
	if err != nil {
		return ViewerResult{}, err
	}
	return ViewerResult{ViewerToken: token, ActiveViewers: count}, nil
}

func (s *Service) ViewerHeartbeat(ctx context.Context, roomID, viewerToken string) (ViewerResult, error) {
	v, err := s.store.GetViewer(ctx, strings.TrimSpace(viewerToken))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ViewerResult{}, ErrNotFound
		}
		return ViewerResult{}, err
	}
	if v.RoomID != roomID {
		return ViewerResult{}, ErrNotFound
	}
	if v.LeftAt != nil {
		return ViewerResult{}, ErrGone
	}

	now := s.now()
	_, err = s.store.UpsertViewer(ctx, repository.UpsertViewerInput{
		ID:              v.ID,
		RoomID:          v.RoomID,
		ViewerToken:     v.ViewerToken,
		JoinedAt:        v.JoinedAt,
		LastHeartbeatAt: now,
		LeftAt:          v.LeftAt,
	})
	if err != nil {
		return ViewerResult{}, err
	}
	count, err := s.store.CountActiveViewers(ctx, roomID, s.now().Add(-s.viewerHeartbeatTimeout))
	if err != nil {
		return ViewerResult{}, err
	}
	return ViewerResult{ActiveViewers: count}, nil
}

func (s *Service) ViewerLeave(ctx context.Context, roomID, viewerToken string) (ViewerResult, error) {
	v, err := s.store.GetViewer(ctx, strings.TrimSpace(viewerToken))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ViewerResult{}, ErrNotFound
		}
		return ViewerResult{}, err
	}
	if v.RoomID != roomID {
		return ViewerResult{}, ErrNotFound
	}
	now := s.now()
	if v.LeftAt == nil {
		v.LeftAt = &now
	}
	_, err = s.store.UpsertViewer(ctx, repository.UpsertViewerInput{
		ID:              v.ID,
		RoomID:          v.RoomID,
		ViewerToken:     v.ViewerToken,
		JoinedAt:        v.JoinedAt,
		LastHeartbeatAt: v.LastHeartbeatAt,
		LeftAt:          v.LeftAt,
	})
	if err != nil {
		return ViewerResult{}, err
	}
	count, err := s.store.CountActiveViewers(ctx, roomID, s.now().Add(-s.viewerHeartbeatTimeout))
	if err != nil {
		return ViewerResult{}, err
	}
	return ViewerResult{ActiveViewers: count}, nil
}

func (s *Service) reconcileRoom(ctx context.Context, rm repository.Room) (repository.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileRoomLocked(ctx, rm)
}

func (s *Service) reconcileRoomLocked(ctx context.Context, rm repository.Room) (repository.Room, error) {
	now := s.now()
	switch rm.State {
	case domain.RoomStateOpen, domain.RoomStateActive:
		if now.After(rm.TTLAt) {
			closed := domain.RoomStateClosed
			var updated repository.Room
			var emitted []repository.RoomEvent
			err := s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
				var txErr error
				updated, txErr = tx.UpdateRoom(ctx, repository.UpdateRoomInput{
					ID:       rm.ID,
					State:    &closed,
					ClosedAt: &now,
				})
				if txErr != nil {
					return txErr
				}
				ev, txErr := tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
					RoomID:    rm.ID,
					EventType: "room.state_changed",
				})
				if txErr != nil {
					return txErr
				}
				emitted = append(emitted, ev)
				ev, txErr = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
					RoomID:    rm.ID,
					EventType: "room.closed",
				})
				if txErr != nil {
					return txErr
				}
				emitted = append(emitted, ev)
				return nil
			})
			if err != nil {
				return repository.Room{}, err
			}
			s.publishRoomEvents(emitted)
			if err := s.upsertRoomContext(ctx, updated, ""); err != nil {
				s.appendAuditEventBestEffort(ctx, rm.ID, "room_context_sync_failed", map[string]any{
					"room_id": rm.ID,
					"reason":  err.Error(),
					"source":  "reconcile_ttl_close",
				}, 0)
			}
			return updated, nil
		}
	case domain.RoomStateClosed:
		closedAt := rm.ClosedAt
		if closedAt == nil {
			updated, err := s.store.UpdateRoom(ctx, repository.UpdateRoomInput{
				ID:       rm.ID,
				ClosedAt: &now,
			})
			if err != nil {
				return repository.Room{}, err
			}
			rm = updated
			if err := s.upsertRoomContext(ctx, rm, ""); err != nil {
				s.appendAuditEventBestEffort(ctx, rm.ID, "room_context_sync_failed", map[string]any{
					"room_id": rm.ID,
					"reason":  err.Error(),
					"source":  "reconcile_closed_at_set",
				}, 0)
			}
			closedAt = rm.ClosedAt
		}
		activeCount, err := s.store.CountActiveViewers(ctx, rm.ID, now.Add(-s.viewerHeartbeatTimeout))
		if err != nil {
			return repository.Room{}, err
		}
		pastGrace := closedAt != nil && now.Sub(*closedAt) >= s.closedRoomGraceDelay
		pastCap := closedAt != nil && now.Sub(*closedAt) >= s.maxClosedRetention
		if (activeCount == 0 && pastGrace) || pastCap {
			msgs, err := s.store.ListRoomMessages(ctx, rm.ID)
			if err != nil {
				return repository.Room{}, err
			}
			var purgedRoom repository.Room
			var emitted []repository.RoomEvent
			err = s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
				if txErr := tx.PurgeRoomContent(ctx, rm.ID, now); txErr != nil {
					return txErr
				}
				ev, txErr := tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
					RoomID:    rm.ID,
					EventType: "room.state_changed",
				})
				if txErr != nil {
					return txErr
				}
				emitted = append(emitted, ev)
				ev, txErr = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
					RoomID:    rm.ID,
					EventType: "room.purged",
				})
				if txErr != nil {
					return txErr
				}
				emitted = append(emitted, ev)
				var getErr error
				purgedRoom, getErr = tx.GetRoom(ctx, rm.ID)
				return getErr
			})
			if err != nil {
				return repository.Room{}, err
			}
			s.publishRoomEvents(emitted)
			if err := s.store.AppendAuditEvent(ctx, repository.AppendAuditEventInput{
				RoomID:       rm.ID,
				Event:        "room_purged",
				Meta:         "content hard-deleted",
				MessageCount: len(msgs),
			}); err != nil {
				return repository.Room{}, err
			}
			return purgedRoom, nil
		}
	}
	return rm, nil
}

func (s *Service) allowAgentMessageLocked(agentID string, now time.Time) bool {
	windowStart := now.Add(-1 * time.Minute)
	timestamps := s.messageWindows[agentID]
	kept := timestamps[:0]
	for _, t := range timestamps {
		if t.After(windowStart) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxMessagesPerMinuteAgent {
		s.messageWindows[agentID] = kept
		return false
	}
	kept = append(kept, now)
	s.messageWindows[agentID] = kept
	return true
}

func (s *Service) allowRoomMessageLocked(roomID string, now time.Time) bool {
	windowStart := now.Add(-1 * time.Minute)
	timestamps := s.roomWindows[roomID]
	kept := timestamps[:0]
	for _, t := range timestamps {
		if t.After(windowStart) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxMessagesPerMinuteRoom {
		s.roomWindows[roomID] = kept
		return false
	}
	kept = append(kept, now)
	s.roomWindows[roomID] = kept
	return true
}

func (s *Service) blockedUntilLocked(agentID string, now time.Time) (time.Time, bool) {
	until, ok := s.blockedAgents[agentID]
	if !ok {
		return time.Time{}, false
	}
	if now.After(until) {
		delete(s.blockedAgents, agentID)
		return time.Time{}, false
	}
	return until, true
}

func (s *Service) recordPolicyViolationLocked(agentID string, now time.Time) int {
	windowStart := now.Add(-policyViolationWindow)
	events := s.policyWindows[agentID]
	kept := events[:0]
	for _, at := range events {
		if at.After(windowStart) {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	s.policyWindows[agentID] = kept
	if len(kept) >= maxPolicyViolationsWindow {
		s.blockedAgents[agentID] = now.Add(policyBlockDuration)
	}
	return len(kept)
}

func newID(prefix string) string {
	return prefix + "_" + randomToken(12)
}

func randomToken(numBytes int) string {
	b := make([]byte, numBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashText(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:])
}

func (s *Service) roomContextFromRoom(rm repository.Room, lastActorID string) roomContextPayload {
	out := roomContextPayload{
		RoomID:       rm.ID,
		AgentAID:     rm.AgentAID,
		AgentBID:     rm.AgentBID,
		LastActorID:  strings.TrimSpace(lastActorID),
		State:        string(rm.State),
		TurnIndex:    rm.TurnIndex,
		MaxTurns:     rm.MaxTurns,
		TTLAt:        rm.TTLAt.Format(time.RFC3339),
		RecentMemory: []recentMemoryEntry{},
	}
	if rm.ClosedAt != nil {
		closed := rm.ClosedAt.Format(time.RFC3339)
		out.ClosedAt = &closed
	}
	return out
}

func (s *Service) formatTaskContext(payload roomContextPayload, agentID string) string {
	lines := []string{
		fmt.Sprintf("room_id=%s", payload.RoomID),
		fmt.Sprintf("self_agent_id=%s", agentID),
		fmt.Sprintf("agent_a_id=%s", payload.AgentAID),
		fmt.Sprintf("agent_b_id=%s", payload.AgentBID),
		fmt.Sprintf("state=%s", payload.State),
		fmt.Sprintf("turn_index=%d", payload.TurnIndex),
		fmt.Sprintf("max_turns=%d", payload.MaxTurns),
		fmt.Sprintf("ttl_at=%s", payload.TTLAt),
	}
	if payload.LastActorID != "" {
		lines = append(lines, fmt.Sprintf("last_actor_id=%s", payload.LastActorID))
	}
	if payload.ClosedAt != nil {
		lines = append(lines, fmt.Sprintf("closed_at=%s", *payload.ClosedAt))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) upsertRoomContext(ctx context.Context, rm repository.Room, lastActorID string) error {
	payload := s.roomContextFromRoom(rm, lastActorID)
	recent, err := s.store.ListRoomMessages(ctx, rm.ID)
	if err != nil {
		return err
	}
	payload.RecentMemory = selectRecentMemory(recent)
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal room context: %w", err)
	}

	version := 1
	current, err := s.store.GetRoomContext(ctx, rm.ID)
	if err == nil {
		version = current.Version + 1
	} else if !errors.Is(err, repository.ErrNotFound) {
		return err
	}

	_, err = s.store.UpsertRoomContext(ctx, repository.UpsertRoomContextInput{
		RoomID:  rm.ID,
		Context: raw,
		Version: version,
	})
	return err
}

func selectRecentMemory(messages []repository.Message) []recentMemoryEntry {
	if len(messages) == 0 {
		return []recentMemoryEntry{}
	}
	if len(messages) > maxRecentMemoryEntries {
		messages = messages[len(messages)-maxRecentMemoryEntries:]
	}
	out := make([]recentMemoryEntry, 0, len(messages))
	for _, m := range messages {
		out = append(out, recentMemoryEntry{
			Turn:     m.Turn,
			SenderID: m.SenderID,
		})
	}
	return out
}

func (s *Service) publishRoomEvents(events []repository.RoomEvent) {
	for _, ev := range events {
		if ev.ID == 0 || strings.TrimSpace(ev.RoomID) == "" {
			continue
		}
		s.emit(ev)
	}
}

func (s *Service) AppendSecurityAudit(ctx context.Context, roomID, event string, meta map[string]any, messageCount int) {
	s.appendAuditEventBestEffort(ctx, roomID, event, meta, messageCount)
}

func (s *Service) appendAuditEventBestEffort(ctx context.Context, roomID, event string, meta map[string]any, messageCount int) {
	if strings.TrimSpace(roomID) == "" || strings.TrimSpace(event) == "" {
		return
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		payload = []byte(`{}`)
	}
	_ = s.store.AppendAuditEvent(ctx, repository.AppendAuditEventInput{
		RoomID:       roomID,
		Event:        event,
		Meta:         string(payload),
		MessageCount: messageCount,
	})
}
