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
	"net"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/febrian/areyouai/internal/domain"
	"github.com/febrian/areyouai/internal/repository"
	"github.com/febrian/areyouai/internal/security"
	"github.com/febrian/areyouai/internal/security/secretcipher"
	"github.com/febrian/areyouai/internal/service/promptbuilder"
)

const (
	maxMessagesPerMinuteAgent       = 30
	maxMessagesPerMinuteRoom        = 60
	policyViolationWindow           = 5 * time.Minute
	policyBlockDuration             = 15 * time.Minute
	maxPolicyViolationsWindow       = 3
	maxRecentMemoryEntries          = 6
	maxContextRecentMessages        = 6
	maxRoomEventHistoryLimit        = 200
	maxRoomContextUpdateTries       = 5
	roomScopedTokenRefreshThreshold = 60 * time.Second
	sessionTTL                      = 14 * 24 * time.Hour
	roomScopedTokenTTL              = 5 * time.Minute
	agentStreamReplayWindow         = 30 * time.Minute
	roomScopeAutomation             = "room:automation"
	roomScopeReadOnly               = "room:read_only"
	humanCodeTTL                    = 24 * time.Hour
)

var (
	ErrBadRequest      = errors.New("bad request")
	ErrUnauthorized    = errors.New("unauthorized")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrTurnMismatch    = errors.New("turn mismatch")
	ErrStaleBundleHash = errors.New("stale bundle hash")
	ErrRoomNotActive   = errors.New("room not active")
	ErrGone            = errors.New("gone")
	ErrRateLimit       = errors.New("rate limit")
	ErrPolicyBlocked   = errors.New("policy blocked")
	ErrPayloadTooLarge = errors.New("payload too large")
)

type Service struct {
	store    repository.Store
	pb       *promptbuilder.Builder
	emit     func(repository.RoomEvent)
	seal     *secretcipher.Cipher
	roomSeal *secretcipher.Cipher

	mu             sync.Mutex
	reconcileMu    sync.Mutex
	reconcileLocks map[string]*sync.Mutex
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
	WebhookSecretKey       string
	WebhookSecretKeyset    string
	RoomDEKKey             string
	RoomDEKKeyset          string
}

type RegisterResult struct {
	AgentID string
	APIKey  string
}

type LoginResult struct {
	SessionToken string
}

type AgentWebhookEndpointResult struct {
	Endpoint repository.AgentWebhookEndpoint
}

type RoomAccessTokenResult struct {
	RoomID    string
	AgentID   string
	Token     string
	Scope     string
	ExpiresAt time.Time
}

type CreateListingResult struct {
	Listing     repository.Listing
	RoomID      string
	HumanCode   string
	OwnerJoined bool
	RoomState   domain.RoomState
	NextActorID string
}

type ConnectResult struct {
	RoomID      string
	HumanCode   string
	AgentAID    string
	AgentBID    string
	RoomState   domain.RoomState
	ListingID   string
	NextTurnA   string
	NextActorID string
}

type ActionableRoomRecovery struct {
	RoomID      string           `json:"room_id"`
	RoomState   domain.RoomState `json:"room_state"`
	NextTurn    int              `json:"next_turn"`
	NextActorID string           `json:"next_actor_id"`
	Token       string           `json:"token"`
	ExpiresAt   time.Time        `json:"expires_at"`
}

type TerminalRoomRecovery struct {
	RoomID    string           `json:"room_id"`
	RoomState domain.RoomState `json:"room_state"`
	ClosedAt  *time.Time       `json:"closed_at,omitempty"`
	PurgedAt  *time.Time       `json:"purged_at,omitempty"`
}

type ActionableRoomsResult struct {
	Actionable []ActionableRoomRecovery `json:"actionable"`
	Terminal   []TerminalRoomRecovery   `json:"terminal"`
}

type AgentStreamResumeResult struct {
	AfterSeq                 int64
	ResumeStatus             string
	LastAcknowledgedDelivery string
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
		seal:                   secretcipher.NewWithKeyset(opts.WebhookSecretKey, opts.WebhookSecretKeyset),
		roomSeal:               secretcipher.NewWithKeyset(opts.RoomDEKKey, opts.RoomDEKKeyset),
		reconcileLocks:         make(map[string]*sync.Mutex),
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

func (s *Service) AuthRoomAccess(ctx context.Context, bearerToken, roomID, action string) (string, error) {
	token := strings.TrimSpace(bearerToken)
	roomID = strings.TrimSpace(roomID)
	if token == "" || roomID == "" {
		return "", ErrUnauthorized
	}

	agentID, err := s.AuthAgentID(ctx, token)
	if err == nil {
		return agentID, nil
	}
	if !errors.Is(err, ErrUnauthorized) {
		return "", err
	}

	scoped, err := s.store.FindRoomScopedTokenByHash(ctx, hashText(token))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", ErrUnauthorized
		}
		return "", err
	}
	if scoped.RevokedAt != nil || !scoped.ExpiresAt.After(s.now()) {
		return "", ErrUnauthorized
	}
	if scoped.RoomID != roomID {
		return "", ErrForbidden
	}
	if !roomScopedTokenAllows(scoped.Scope, action) {
		return "", ErrForbidden
	}
	if shouldRefreshRoomScopedToken(scoped.ExpiresAt, s.now()) {
		if err := s.store.TouchRoomScopedToken(ctx, scoped.TokenHash, s.now(), s.now().Add(roomScopedTokenTTL)); err != nil {
			return "", err
		}
	}
	return scoped.AgentID, nil
}

func shouldRefreshRoomScopedToken(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() {
		return true
	}
	return expiresAt.Sub(now) <= roomScopedTokenRefreshThreshold
}

func (s *Service) CreateRoomAccessToken(ctx context.Context, agentID, roomID string) (RoomAccessTokenResult, error) {
	rm, err := s.store.GetRoom(ctx, strings.TrimSpace(roomID))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return RoomAccessTokenResult{}, ErrNotFound
		}
		return RoomAccessTokenResult{}, err
	}
	rm, err = s.reconcileRoom(ctx, rm)
	if err != nil {
		return RoomAccessTokenResult{}, err
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		return RoomAccessTokenResult{}, ErrForbidden
	}
	if rm.State != domain.RoomStateOpen && rm.State != domain.RoomStateActive {
		return RoomAccessTokenResult{}, ErrGone
	}

	return s.issueRoomAccessToken(ctx, agentID, rm)
}

func (s *Service) issueRoomAccessToken(ctx context.Context, agentID string, rm repository.Room) (RoomAccessTokenResult, error) {
	now := s.now()
	plainToken := "rat_" + randomToken(24)
	scope := roomScopeAutomation
	expiresAt := now.Add(roomScopedTokenTTL)
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
		if revokeErr := tx.RevokeRoomScopedTokens(ctx, rm.ID, agentID, now); revokeErr != nil {
			return revokeErr
		}
		_, createErr := tx.CreateRoomScopedToken(ctx, repository.CreateRoomScopedTokenInput{
			ID:        newID("rat"),
			RoomID:    rm.ID,
			AgentID:   agentID,
			TokenHash: hashText(plainToken),
			Scope:     scope,
			ExpiresAt: expiresAt,
		})
		return createErr
	}); err != nil {
		return RoomAccessTokenResult{}, err
	}

	return RoomAccessTokenResult{
		RoomID:    rm.ID,
		AgentID:   agentID,
		Token:     plainToken,
		Scope:     scope,
		ExpiresAt: expiresAt,
	}, nil
}

func (s *Service) ActionableRooms(ctx context.Context, agentID string) (ActionableRoomsResult, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return ActionableRoomsResult{}, ErrUnauthorized
	}

	rooms, err := s.store.ListRecoverableRoomsForAgent(ctx, agentID, s.now().Add(-agentStreamReplayWindow))
	if err != nil {
		return ActionableRoomsResult{}, err
	}

	out := ActionableRoomsResult{
		Actionable: []ActionableRoomRecovery{},
		Terminal:   []TerminalRoomRecovery{},
	}
	seenTerminal := map[string]bool{}
	for _, candidate := range rooms {
		rm, reconcileErr := s.reconcileRoom(ctx, candidate)
		if reconcileErr != nil {
			return ActionableRoomsResult{}, reconcileErr
		}
		if rm.AgentAID != agentID && rm.AgentBID != agentID {
			continue
		}

		switch rm.State {
		case domain.RoomStateActive:
			if nextActorIDForRoom(rm) != agentID {
				continue
			}
			token, tokenErr := s.issueRoomAccessToken(ctx, agentID, rm)
			if tokenErr != nil {
				if errors.Is(tokenErr, ErrGone) {
					continue
				}
				return ActionableRoomsResult{}, tokenErr
			}
			out.Actionable = append(out.Actionable, ActionableRoomRecovery{
				RoomID:      rm.ID,
				RoomState:   rm.State,
				NextTurn:    rm.TurnIndex,
				NextActorID: nextActorIDForRoom(rm),
				Token:       token.Token,
				ExpiresAt:   token.ExpiresAt,
			})
		case domain.RoomStateClosed, domain.RoomStatePurged:
			if seenTerminal[rm.ID] {
				continue
			}
			seenTerminal[rm.ID] = true
			out.Terminal = append(out.Terminal, TerminalRoomRecovery{
				RoomID:    rm.ID,
				RoomState: rm.State,
				ClosedAt:  rm.ClosedAt,
				PurgedAt:  rm.PurgedAt,
			})
		}
	}
	return out, nil
}

func (s *Service) ResolveAgentStreamResume(ctx context.Context, agentID, lastDeliveryID string) (AgentStreamResumeResult, error) {
	lastDeliveryID = strings.TrimSpace(lastDeliveryID)
	if lastDeliveryID == "" {
		return AgentStreamResumeResult{
			AfterSeq:                 0,
			ResumeStatus:             "fresh",
			LastAcknowledgedDelivery: "",
		}, nil
	}
	delivery, err := s.store.GetAgentStreamDelivery(ctx, strings.TrimSpace(agentID), lastDeliveryID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return AgentStreamResumeResult{
				ResumeStatus:             "replay_required",
				LastAcknowledgedDelivery: "",
			}, nil
		}
		return AgentStreamResumeResult{}, err
	}
	if !delivery.ExpiresAt.After(s.now()) {
		return AgentStreamResumeResult{
			ResumeStatus:             "replay_required",
			LastAcknowledgedDelivery: "",
		}, nil
	}
	if !strings.EqualFold(strings.TrimSpace(delivery.Status), "acked") {
		return AgentStreamResumeResult{
			ResumeStatus:             "replay_required",
			LastAcknowledgedDelivery: "",
		}, nil
	}
	return AgentStreamResumeResult{
		AfterSeq:                 delivery.Seq,
		ResumeStatus:             "ok",
		LastAcknowledgedDelivery: delivery.DeliveryID,
	}, nil
}

func (s *Service) ListPendingAgentStreamDeliveries(ctx context.Context, agentID string, afterSeq int64, limit int) ([]repository.AgentStreamDelivery, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, ErrUnauthorized
	}
	return s.store.ListPendingAgentStreamDeliveries(ctx, strings.TrimSpace(agentID), afterSeq, s.now(), limit)
}

func (s *Service) AckAgentStreamDelivery(ctx context.Context, agentID, deliveryID string) error {
	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(deliveryID) == "" {
		return ErrBadRequest
	}
	if err := s.store.AckAgentStreamDelivery(ctx, strings.TrimSpace(agentID), strings.TrimSpace(deliveryID), s.now()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s *Service) CreateAgentWebhookEndpoint(ctx context.Context, agentID, rawURL, secret, keyID string, enabled bool) (AgentWebhookEndpointResult, error) {
	rawURL = strings.TrimSpace(rawURL)
	secret = strings.TrimSpace(secret)
	keyID = strings.TrimSpace(keyID)
	if rawURL == "" || secret == "" {
		return AgentWebhookEndpointResult{}, ErrBadRequest
	}
	normalizedURL, err := normalizeWebhookEndpointURL(rawURL)
	if err != nil {
		return AgentWebhookEndpointResult{}, ErrBadRequest
	}
	if keyID == "" {
		keyID = "v1"
	}
	sealedSecret, err := s.seal.Encrypt(secret)
	if err != nil {
		return AgentWebhookEndpointResult{}, err
	}
	out, err := s.store.CreateAgentWebhookEndpoint(ctx, repository.CreateAgentWebhookEndpointInput{
		ID:               newID("whk"),
		AgentID:          strings.TrimSpace(agentID),
		URL:              normalizedURL,
		SecretCiphertext: sealedSecret,
		KeyID:            keyID,
		Enabled:          enabled,
	})
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return AgentWebhookEndpointResult{}, ErrConflict
		}
		return AgentWebhookEndpointResult{}, err
	}
	return AgentWebhookEndpointResult{Endpoint: out}, nil
}

func (s *Service) ListAgentWebhookEndpoints(ctx context.Context, agentID string) ([]repository.AgentWebhookEndpoint, error) {
	return s.store.ListAgentWebhookEndpoints(ctx, strings.TrimSpace(agentID))
}

func (s *Service) DeleteAgentWebhookEndpoint(ctx context.Context, agentID, endpointID string) error {
	if strings.TrimSpace(endpointID) == "" {
		return ErrBadRequest
	}
	if err := s.store.DeleteAgentWebhookEndpoint(ctx, strings.TrimSpace(agentID), strings.TrimSpace(endpointID)); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s *Service) CreateListing(ctx context.Context, agentID, topic string, tags []string, maxTurns, ttlSeconds int) (CreateListingResult, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return CreateListingResult{}, ErrBadRequest
	}
	if maxTurns <= 0 {
		maxTurns = 20
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 900
	}

	now := s.now()
	roomID := newID("room")
	humanCode := "hc_" + randomToken(18)
	result := CreateListingResult{}
	var createdRoom repository.Room
	var emitted []repository.RoomEvent

	err := s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
		humanCodeExpiresAt := now.Add(humanCodeTTL)
		messageKeyCiphertext, _, err := s.newRoomMessageCipher()
		if err != nil {
			return err
		}
		rm, err := tx.CreateRoom(ctx, repository.CreateRoomInput{
			ID:                   roomID,
			Topic:                topic,
			AgentAID:             agentID,
			AgentBID:             "",
			State:                domain.RoomStateOpen,
			TurnIndex:            0,
			MaxTurns:             maxTurns,
			TTLAt:                now.Add(time.Duration(ttlSeconds) * time.Second),
			HumanCodeHash:        hashText(humanCode),
			HumanCodeExpiresAt:   &humanCodeExpiresAt,
			MessageKeyCiphertext: messageKeyCiphertext,
		})
		if err != nil {
			return err
		}
		createdRoom = rm

		item, err := tx.CreateListing(ctx, repository.CreateListingInput{
			ID:         newID("lst"),
			AgentID:    agentID,
			Topic:      topic,
			Tags:       tags,
			MaxTurns:   maxTurns,
			TTLSeconds: ttlSeconds,
			RoomID:     roomID,
		})
		if err != nil {
			return err
		}

		ev, err := tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
			RoomID:    roomID,
			EventType: "room.created",
			SenderID:  &agentID,
		})
		if err != nil {
			return err
		}
		emitted = append(emitted, ev)
		if err := s.enqueueWebhookOutboxForEvent(ctx, tx, rm, ev, agentID); err != nil {
			return err
		}

		ev, err = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
			RoomID:    roomID,
			EventType: "room.joined",
			SenderID:  &agentID,
		})
		if err != nil {
			return err
		}
		emitted = append(emitted, ev)
		if err := s.enqueueWebhookOutboxForEvent(ctx, tx, rm, ev, agentID); err != nil {
			return err
		}

		result = CreateListingResult{
			Listing:     item,
			RoomID:      roomID,
			HumanCode:   humanCode,
			OwnerJoined: true,
			RoomState:   rm.State,
			NextActorID: rm.AgentAID,
		}
		return nil
	})
	if err != nil {
		return CreateListingResult{}, err
	}

	s.publishRoomEvents(emitted)
	s.syncRoomContextBestEffort(ctx, createdRoom, "create_listing", 0)
	return result, nil
}

func (s *Service) SearchListings(ctx context.Context, query string) ([]repository.Listing, error) {
	return s.store.SearchListings(ctx, strings.TrimSpace(strings.ToLower(query)))
}

func (s *Service) ConnectListing(ctx context.Context, agentID, listingID string) (ConnectResult, error) {
	res := ConnectResult{}
	var updatedRoom repository.Room
	var emitted []repository.RoomEvent

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

		if strings.TrimSpace(l.RoomID) == "" {
			humanCode := "hc_" + randomToken(18)
			now := s.now()
			humanCodeExpiresAt := now.Add(humanCodeTTL)
			active := domain.RoomStateActive
			messageKeyCiphertext, _, err := s.newRoomMessageCipher()
			if err != nil {
				return err
			}
			rm, err := tx.CreateRoom(ctx, repository.CreateRoomInput{
				ID:                   newID("room"),
				Topic:                l.Topic,
				AgentAID:             l.AgentID,
				AgentBID:             agentID,
				State:                active,
				TurnIndex:            0,
				MaxTurns:             l.MaxTurns,
				TTLAt:                now.Add(time.Duration(l.TTLSeconds) * time.Second),
				HumanCodeHash:        hashText(humanCode),
				HumanCodeExpiresAt:   &humanCodeExpiresAt,
				MessageKeyCiphertext: messageKeyCiphertext,
			})
			if err != nil {
				return err
			}
			ev, err := tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
				RoomID:    rm.ID,
				EventType: "room.joined",
				SenderID:  &agentID,
			})
			if err != nil {
				return err
			}
			emitted = append(emitted, ev)
			if err := s.enqueueWebhookOutboxForEvent(ctx, tx, rm, ev, agentID); err != nil {
				return err
			}
			if err := s.enqueueAgentStreamTurnReady(ctx, tx, rm, "room_activated", eventTime(ev, now)); err != nil {
				return err
			}
			ev, err = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
				RoomID:    rm.ID,
				EventType: "room.state_changed",
				SenderID:  &agentID,
			})
			if err != nil {
				return err
			}
			emitted = append(emitted, ev)
			updatedRoom = rm
			res = ConnectResult{
				RoomID:      rm.ID,
				HumanCode:   humanCode,
				AgentAID:    rm.AgentAID,
				AgentBID:    rm.AgentBID,
				RoomState:   rm.State,
				ListingID:   l.ID,
				NextTurnA:   rm.AgentAID,
				NextActorID: rm.AgentAID,
			}
			return nil
		}

		rm, err := tx.GetRoom(ctx, l.RoomID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ErrNotFound
			}
			return err
		}
		if strings.TrimSpace(rm.AgentBID) != "" {
			return ErrConflict
		}
		topic := l.Topic
		nextState := domain.RoomStateActive
		rm, err = tx.UpdateRoom(ctx, repository.UpdateRoomInput{
			ID:       rm.ID,
			Topic:    &topic,
			AgentBID: &agentID,
			State:    &nextState,
		})
		if err != nil {
			return err
		}
		updatedRoom = rm

		ev, err := tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
			RoomID:    rm.ID,
			EventType: "room.joined",
			SenderID:  &agentID,
		})
		if err != nil {
			return err
		}
		emitted = append(emitted, ev)
		if err := s.enqueueWebhookOutboxForEvent(ctx, tx, rm, ev, agentID); err != nil {
			return err
		}
		if err := s.enqueueAgentStreamTurnReady(ctx, tx, rm, "room_activated", eventTime(ev, s.now())); err != nil {
			return err
		}

		ev, err = tx.AppendRoomEvent(ctx, repository.AppendRoomEventInput{
			RoomID:    rm.ID,
			EventType: "room.state_changed",
			SenderID:  &agentID,
		})
		if err != nil {
			return err
		}
		emitted = append(emitted, ev)

		res = ConnectResult{
			RoomID:      rm.ID,
			HumanCode:   "",
			AgentAID:    rm.AgentAID,
			AgentBID:    rm.AgentBID,
			RoomState:   rm.State,
			ListingID:   l.ID,
			NextTurnA:   rm.AgentAID,
			NextActorID: rm.AgentAID,
		}
		return nil
	})
	if err != nil {
		return ConnectResult{}, err
	}

	s.publishRoomEvents(emitted)
	s.syncRoomContextBestEffort(ctx, updatedRoom, "connect_listing", 0)
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
	joined := buildJoinedMap(rm, s.joined[roomID])
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
			if err := s.enqueueAgentStreamTurnReady(ctx, tx, room, "room_activated", eventTime(ev, s.now())); err != nil {
				return err
			}
			updatedRoom = room
			return nil
		})
		if err != nil {
			return "", nil, err
		}
		s.publishRoomEvents(emitted)
		if err := s.upsertRoomContext(ctx, updatedRoom, ""); err != nil {
			s.appendAuditEventBestEffort(ctx, updatedRoom.ID, "room_context_sync_failed", map[string]any{
				"room_id": rm.ID,
				"reason":  err.Error(),
				"source":  "join_room_activate",
			}, 0)
		}
		return next, joined, nil
	}
	if err := s.upsertRoomContext(ctx, rm, ""); err != nil {
		s.appendAuditEventBestEffort(ctx, rm.ID, "room_context_sync_failed", map[string]any{
			"room_id": rm.ID,
			"reason":  err.Error(),
			"source":  "join_room",
		}, 0)
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
	OrderedStack    []string
	SystemCoreHash  string
	GlobalRulesHash string
	AgentRulesHash  string
	IdentityHash    string
	SoulHash        string
	UserHash        string
	Prompt          string
	NextTurn        int
	NextActorID     string
}

type roomContextPayload struct {
	RoomID                      string              `json:"room_id"`
	Topic                       string              `json:"topic,omitempty"`
	ConversationMode            string              `json:"conversation_mode,omitempty"`
	ConversationSummary         string              `json:"conversation_summary,omitempty"`
	AgentAID                    string              `json:"agent_a_id"`
	AgentBID                    string              `json:"agent_b_id"`
	LastActorID                 string              `json:"last_actor_id,omitempty"`
	LastContextFetchTurnByAgent map[string]int      `json:"last_context_fetch_turn_by_agent,omitempty"`
	State                       string              `json:"state"`
	TurnIndex                   int                 `json:"turn_index"`
	MaxTurns                    int                 `json:"max_turns"`
	TTLAt                       string              `json:"ttl_at"`
	ClosedAt                    *string             `json:"closed_at,omitempty"`
	RecentMemory                []recentMemoryEntry `json:"recent_memory"`
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
	if rm.State != domain.RoomStateActive {
		return SendMessageResult{}, ErrRoomNotActive
	}
	if s.now().After(rm.TTLAt) {
		return SendMessageResult{}, ErrGone
	}
	now := s.now()
	sharedCoordination := supportsMultiInstanceCoordination(s.store)
	if !sharedCoordination {
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
	}
	if expectedTurn != rm.TurnIndex {
		return SendMessageResult{}, ErrTurnMismatch
	}

	expectedSender := rm.AgentAID
	if rm.TurnIndex%2 == 1 {
		expectedSender = rm.AgentBID
	}
	if expectedSender != agentID {
		return SendMessageResult{}, ErrTurnMismatch
	}

	decision := security.EvaluateMessageForPersist(ciphertext)
	if !decision.Allowed && decision.Code == "payload_too_large" {
		s.appendAuditEventBestEffort(ctx, roomID, "message_policy_blocked", map[string]any{
			"room_id":     roomID,
			"agent_id":    agentID,
			"code":        decision.Code,
			"reason":      decision.Reason,
			"max_chars":   security.MaxPersistMessageChars,
			"turn_index":  rm.TurnIndex,
			"bundle_hash": providedBundleHash,
		}, 0)
		return SendMessageResult{}, ErrPayloadTooLarge
	}

	bundle, recentCount, err := s.buildBundleForRoom(ctx, rm, agentID)
	if err != nil {
		return SendMessageResult{}, err
	}
	if !sharedCoordination {
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
		if !strings.EqualFold(bundle.BundleHash, providedBundleHash) {
			s.appendAuditEventBestEffort(ctx, roomID, "bundle_hash_mismatch", map[string]any{
				"room_id":       roomID,
				"agent_id":      agentID,
				"expected_hash": bundle.BundleHash,
				"provided_hash": providedBundleHash,
				"turn_index":    rm.TurnIndex,
			}, recentCount)
			return SendMessageResult{}, ErrStaleBundleHash
		}
	}

	var msg repository.Message
	nextTurn := rm.TurnIndex + 1
	nextState := rm.State
	var updatedRoom repository.Room
	var emitted []repository.RoomEvent
	err = s.store.WithTx(ctx, func(ctx context.Context, tx repository.TxStore) error {
		messageCipher, encErr := s.roomMessageCipher(rm)
		if encErr != nil {
			return encErr
		}
		if messageCipher == nil {
			rm, messageCipher, encErr = s.ensureRoomMessageCipher(ctx, tx, rm)
			if encErr != nil {
				return encErr
			}
		}
		storedCiphertext := ciphertext
		if messageCipher != nil {
			storedCiphertext, encErr = messageCipher.Encrypt(ciphertext)
			if encErr != nil {
				return encErr
			}
		}
		if sharedCoordination {
			if err := lockSharedMessageCoordination(ctx, tx, roomID, agentID); err != nil {
				return err
			}
			if policyStore, ok := tx.(repository.AgentPolicyStore); ok {
				if until, blocked, err := policyStore.GetAgentPolicyBlock(ctx, agentID, now); err != nil {
					return err
				} else if blocked {
					s.appendAuditEventBestEffort(ctx, roomID, "message_policy_blocked", map[string]any{
						"room_id":       roomID,
						"agent_id":      agentID,
						"code":          "agent_temporarily_blocked",
						"reason":        "too many policy violations",
						"blocked_until": until.Format(time.RFC3339),
					}, 0)
					return ErrPolicyBlocked
				}
			}
			if counter, ok := tx.(repository.MessageCounterStore); ok {
				agentCount, err := counter.CountMessagesBySenderSince(ctx, agentID, now.Add(-1*time.Minute))
				if err != nil {
					return err
				}
				if agentCount >= maxMessagesPerMinuteAgent {
					s.appendAuditEventBestEffort(ctx, roomID, "message_rate_limited", map[string]any{
						"room_id":  roomID,
						"agent_id": agentID,
						"scope":    "agent",
					}, 0)
					return ErrRateLimit
				}
				roomCount, err := counter.CountMessagesByRoomSince(ctx, roomID, now.Add(-1*time.Minute))
				if err != nil {
					return err
				}
				if roomCount >= maxMessagesPerMinuteRoom {
					s.appendAuditEventBestEffort(ctx, roomID, "message_rate_limited", map[string]any{
						"room_id":  roomID,
						"agent_id": agentID,
						"scope":    "room",
					}, 0)
					return ErrRateLimit
				}
			}
			if !decision.Allowed {
				state := repository.PolicyState{}
				if policyStore, ok := tx.(repository.AgentPolicyStore); ok {
					var recordErr error
					state, recordErr = policyStore.RecordAgentPolicyViolation(ctx, agentID, now, policyViolationWindow, policyBlockDuration, maxPolicyViolationsWindow)
					if recordErr != nil {
						return recordErr
					}
				}
				s.appendAuditEventBestEffort(ctx, roomID, "message_policy_blocked", map[string]any{
					"room_id":          roomID,
					"agent_id":         agentID,
					"code":             decision.Code,
					"reason":           decision.Reason,
					"violation_count":  state.ViolationCount,
					"blocked_temporal": state.BlockedUntil != nil,
				}, 0)
				return ErrPolicyBlocked
			}
			if !strings.EqualFold(bundle.BundleHash, providedBundleHash) {
				s.appendAuditEventBestEffort(ctx, roomID, "bundle_hash_mismatch", map[string]any{
					"room_id":       roomID,
					"agent_id":      agentID,
					"expected_hash": bundle.BundleHash,
					"provided_hash": providedBundleHash,
					"turn_index":    rm.TurnIndex,
				}, recentCount)
				return ErrStaleBundleHash
			}
		}
		var txErr error
		msg, txErr = tx.AppendMessage(ctx, repository.AppendMessageInput{
			ID:         newID("msg"),
			RoomID:     roomID,
			SenderID:   agentID,
			Turn:       rm.TurnIndex,
			Ciphertext: storedCiphertext,
		})
		if txErr != nil {
			if errors.Is(txErr, repository.ErrConflict) {
				return ErrTurnMismatch
			}
			return txErr
		}
		msg.Ciphertext = ciphertext

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
		if ev.Ciphertext != nil {
			*ev.Ciphertext = ciphertext
		}
		emitted = append(emitted, ev)
		if err := s.enqueueWebhookOutboxForEvent(ctx, tx, updatedRoom, ev, agentID); err != nil {
			return err
		}
		if nextState == domain.RoomStateActive {
			if err := s.enqueueAgentStreamTurnReady(ctx, tx, updatedRoom, "peer_message", eventTime(ev, now)); err != nil {
				return err
			}
		}

		if nextState == domain.RoomStateClosed {
			if err := revokeRoomScopedTokensForRoom(ctx, tx, updatedRoom, s.now()); err != nil {
				return err
			}
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
			if err := s.enqueueWebhookOutboxForEvent(ctx, tx, updatedRoom, ev, agentID); err != nil {
				return err
			}
			if err := s.enqueueAgentStreamTerminal(ctx, tx, updatedRoom, "room.closed", "max_turns_reached", eventTime(ev, now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTurnMismatch) {
			return SendMessageResult{}, ErrTurnMismatch
		}
		if errors.Is(err, ErrStaleBundleHash) {
			return SendMessageResult{}, ErrStaleBundleHash
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
		"identity_hash":     bundle.IdentityHash,
		"soul_hash":         bundle.SoulHash,
		"user_hash":         bundle.UserHash,
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
		OrderedStack:    bundle.OrderedStack,
		SystemCoreHash:  bundle.SystemCoreHash,
		GlobalRulesHash: bundle.GlobalRulesHash,
		AgentRulesHash:  bundle.AgentRulesHash,
		IdentityHash:    bundle.IdentityHash,
		SoulHash:        bundle.SoulHash,
		UserHash:        bundle.UserHash,
		Prompt:          bundle.Prompt,
		NextTurn:        rm.TurnIndex,
		NextActorID:     nextActorIDForRoom(rm),
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
		s.appendAuditEventBestEffort(ctx, rm.ID, "room_context_read_failed", map[string]any{
			"room_id": rm.ID,
			"reason":  err.Error(),
			"source":  "context_bundle",
		}, 0)
	}

	recent, listErr := s.listRecentRoomMessages(ctx, rm.ID, maxContextRecentMessages)
	if listErr != nil {
		return promptbuilder.Bundle{}, 0, listErr
	}
	recent, listErr = s.decryptRoomMessages(rm, recent)
	if listErr != nil {
		return promptbuilder.Bundle{}, 0, listErr
	}
	payload.ConversationSummary = buildConversationSummary(payload.Topic, payload.ConversationMode, recent)
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

type recentRoomMessagesLister interface {
	ListRecentRoomMessages(ctx context.Context, roomID string, limit int) ([]repository.Message, error)
}

func (s *Service) listRecentRoomMessages(ctx context.Context, roomID string, limit int) ([]repository.Message, error) {
	if lister, ok := s.store.(recentRoomMessagesLister); ok {
		return lister.ListRecentRoomMessages(ctx, roomID, limit)
	}
	return s.store.ListRoomMessages(ctx, roomID)
}

type RoomStateResult struct {
	Room          repository.Room
	ActiveViewers int
	NextTurn      int
	NextActorID   string
}

type RoomEventHistoryResult struct {
	Items     []repository.RoomEvent
	NextSince int64
}

type AdminOverviewResult struct {
	Overview repository.AdminOverview
	Purge    PurgeOverviewResult
}

type LifecycleSweepResult struct {
	Scanned           int
	ClosedTransitions int
	PurgedTransitions int
	ViewerBlocked     int
	ReadyForPurge     int
}

type PurgeOverviewResult struct {
	Scanned                   int
	ClosedRooms               int
	ReadyForPurge             int
	ViewerBlocked             int
	OverRetention             int
	OldestClosedAgeSeconds    int
	OldestReadyAgeSeconds     int
	ClosedRoomGraceSeconds    int
	MaxClosedRetentionSeconds int
}

func (s *Service) AdminOverview(ctx context.Context) (AdminOverviewResult, error) {
	out, err := s.store.GetAdminOverview(ctx, s.now())
	if err != nil {
		return AdminOverviewResult{}, err
	}
	purge, err := s.PurgeOverview(ctx)
	if err != nil {
		return AdminOverviewResult{}, err
	}
	return AdminOverviewResult{Overview: out, Purge: purge}, nil
}

func (s *Service) AdminRooms(ctx context.Context, limit int) ([]repository.AdminRoom, error) {
	return s.store.ListAdminRooms(ctx, limit)
}

func (s *Service) AdminAudit(ctx context.Context, limit int) ([]repository.AuditEvent, error) {
	return s.store.ListAuditEvents(ctx, limit)
}

func (s *Service) PurgeOverview(ctx context.Context) (PurgeOverviewResult, error) {
	sweepStore, ok := s.store.(repository.RoomLifecycleSweepStore)
	if !ok {
		return PurgeOverviewResult{}, fmt.Errorf("purge overview unsupported by backing store")
	}
	rooms, err := sweepStore.ListRoomsForLifecycleSweep(ctx, s.now(), 1000)
	if err != nil {
		return PurgeOverviewResult{}, err
	}
	return s.purgeOverviewFromRooms(ctx, rooms), nil
}

func (s *Service) purgeOverviewFromRooms(ctx context.Context, rooms []repository.Room) PurgeOverviewResult {
	now := s.now()
	out := PurgeOverviewResult{
		Scanned:                   len(rooms),
		ClosedRoomGraceSeconds:    int(s.closedRoomGraceDelay.Seconds()),
		MaxClosedRetentionSeconds: int(s.maxClosedRetention.Seconds()),
	}
	for _, rm := range rooms {
		if rm.State != domain.RoomStateClosed {
			continue
		}
		out.ClosedRooms++
		closedAt := rm.ClosedAt
		if closedAt == nil {
			closedAt = &rm.CreatedAt
		}
		age := now.Sub(*closedAt)
		if age < 0 {
			age = 0
		}
		if secs := int(age.Seconds()); secs > out.OldestClosedAgeSeconds {
			out.OldestClosedAgeSeconds = secs
		}
		activeCount, err := s.store.CountActiveViewers(ctx, rm.ID, now.Add(-s.viewerHeartbeatTimeout))
		if err != nil {
			continue
		}
		viewerBlocked := activeCount > 0
		eligibleByGrace := !viewerBlocked && age >= s.closedRoomGraceDelay
		eligibleByRetention := age >= s.maxClosedRetention
		if viewerBlocked {
			out.ViewerBlocked++
		}
		if eligibleByGrace || eligibleByRetention {
			out.ReadyForPurge++
			if secs := int(age.Seconds()); secs > out.OldestReadyAgeSeconds {
				out.OldestReadyAgeSeconds = secs
			}
		}
		if eligibleByRetention {
			out.OverRetention++
		}
	}
	return out
}

func (s *Service) SweepLifecycle(ctx context.Context, limit int) (LifecycleSweepResult, error) {
	sweepStore, ok := s.store.(repository.RoomLifecycleSweepStore)
	if !ok {
		return LifecycleSweepResult{}, fmt.Errorf("lifecycle sweep unsupported by backing store")
	}
	rooms, err := sweepStore.ListRoomsForLifecycleSweep(ctx, s.now(), limit)
	if err != nil {
		return LifecycleSweepResult{}, err
	}

	out := LifecycleSweepResult{Scanned: len(rooms)}
	for _, candidate := range rooms {
		before := candidate.State
		after, reconcileErr := s.reconcileRoom(ctx, candidate)
		if reconcileErr != nil {
			return out, reconcileErr
		}
		if after.State == domain.RoomStateClosed {
			activeCount, err := s.store.CountActiveViewers(ctx, after.ID, s.now().Add(-s.viewerHeartbeatTimeout))
			if err != nil {
				return out, err
			}
			closedAt := after.ClosedAt
			if closedAt == nil {
				closedAt = &after.CreatedAt
			}
			age := s.now().Sub(*closedAt)
			if age < 0 {
				age = 0
			}
			if activeCount > 0 {
				out.ViewerBlocked++
			}
			if (activeCount == 0 && age >= s.closedRoomGraceDelay) || age >= s.maxClosedRetention {
				out.ReadyForPurge++
			}
		}
		if before == after.State {
			continue
		}
		if after.State == domain.RoomStateClosed {
			out.ClosedTransitions++
		}
		if after.State == domain.RoomStatePurged {
			out.PurgedTransitions++
		}
	}
	return out, nil
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
	joined := s.joined[roomID]
	s.mu.Unlock()
	if !roomParticipantJoined(rm, agentID, joined) {
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
	items, err = s.decryptRoomEvents(rm, items)
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
	return RoomStateResult{
		Room:          rm,
		ActiveViewers: count,
		NextTurn:      rm.TurnIndex,
		NextActorID:   nextActorIDForRoom(rm),
	}, nil
}

func (s *Service) RoomSnapshot(ctx context.Context, roomID string) (repository.Room, error) {
	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return repository.Room{}, ErrNotFound
		}
		return repository.Room{}, err
	}
	return s.reconcileRoom(ctx, rm)
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
		if roomLocker, ok := tx.(repository.RoomLockStore); ok {
			if err := roomLocker.LockRoom(ctx, roomID); err != nil {
				return err
			}
		}
		current, currentErr := tx.GetRoom(ctx, roomID)
		if currentErr != nil {
			return currentErr
		}
		if current.State == domain.RoomStatePurged {
			return ErrGone
		}
		if current.State == domain.RoomStateClosed {
			updatedRoom = current
			return nil
		}
		room, txErr := tx.UpdateRoom(ctx, repository.UpdateRoomInput{
			ID:       roomID,
			State:    &closed,
			ClosedAt: &now,
		})
		if txErr != nil {
			return txErr
		}
		if err := revokeRoomScopedTokensForRoom(ctx, tx, room, now); err != nil {
			return err
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
		if err := s.enqueueWebhookOutboxForEvent(ctx, tx, room, ev, agentID); err != nil {
			return err
		}
		if err := s.enqueueAgentStreamTerminal(ctx, tx, room, "room.closed", "manual_close", eventTime(ev, now)); err != nil {
			return err
		}
		updatedRoom = room
		return nil
	})
	if err != nil {
		return repository.Room{}, err
	}
	s.publishRoomEvents(emitted)
	if err := s.upsertRoomContext(ctx, updatedRoom, agentID); err != nil {
		s.appendAuditEventBestEffort(ctx, updatedRoom.ID, "room_context_sync_failed", map[string]any{
			"room_id":  updatedRoom.ID,
			"agent_id": agentID,
			"reason":   err.Error(),
			"source":   "close_room",
		}, 0)
	}
	return updatedRoom, nil
}

type TranscriptResult struct {
	Room                    repository.Room
	Messages                []repository.Message
	LastContextFetchByAgent map[string]int
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
	if rm.HumanCodeExpiresAt != nil && s.now().After(*rm.HumanCodeExpiresAt) {
		return TranscriptResult{}, ErrForbidden
	}

	msgs, err := s.store.ListRoomMessages(ctx, roomID)
	if err != nil {
		return TranscriptResult{}, err
	}
	msgs, err = s.decryptRoomMessages(rm, msgs)
	if err != nil {
		return TranscriptResult{}, err
	}
	result := TranscriptResult{Room: rm, Messages: msgs}
	if contextState, err := s.store.GetRoomContext(ctx, roomID); err == nil {
		var persisted roomContextPayload
		if unmarshalErr := json.Unmarshal(contextState.Context, &persisted); unmarshalErr == nil {
			result.LastContextFetchByAgent = cloneIntMap(persisted.LastContextFetchTurnByAgent)
		}
	}
	return result, nil
}

func (s *Service) RecordRoomContextFetch(ctx context.Context, agentID, roomID string, turnIndex int) error {
	if turnIndex < 0 {
		return ErrBadRequest
	}
	return s.updateRoomContext(ctx, roomID, "", func(rm repository.Room, payload roomContextPayload) (roomContextPayload, error) {
		if rm.AgentAID != agentID && rm.AgentBID != agentID {
			return roomContextPayload{}, ErrForbidden
		}
		if payload.LastContextFetchTurnByAgent == nil {
			payload.LastContextFetchTurnByAgent = make(map[string]int)
		}
		if current, ok := payload.LastContextFetchTurnByAgent[agentID]; !ok || turnIndex > current {
			payload.LastContextFetchTurnByAgent[agentID] = turnIndex
		}
		return payload, nil
	})
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
	if rm.HumanCodeExpiresAt != nil && s.now().After(*rm.HumanCodeExpiresAt) {
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
	lock := s.reconcileLockForRoom(rm.ID)
	lock.Lock()
	defer lock.Unlock()
	return s.reconcileRoomLocked(ctx, rm)
}

func (s *Service) reconcileLockForRoom(roomID string) *sync.Mutex {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		roomID = "_"
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if s.reconcileLocks == nil {
		s.reconcileLocks = make(map[string]*sync.Mutex)
	}
	if lock, ok := s.reconcileLocks[roomID]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	s.reconcileLocks[roomID] = lock
	return lock
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
				if roomLocker, ok := tx.(repository.RoomLockStore); ok {
					if err := roomLocker.LockRoom(ctx, rm.ID); err != nil {
						return err
					}
				}
				var txErr error
				current, currentErr := tx.GetRoom(ctx, rm.ID)
				if currentErr != nil {
					return currentErr
				}
				if current.State == domain.RoomStateClosed || current.State == domain.RoomStatePurged {
					updated = current
					return nil
				}
				updated, txErr = tx.UpdateRoom(ctx, repository.UpdateRoomInput{
					ID:       rm.ID,
					State:    &closed,
					ClosedAt: &now,
				})
				if txErr != nil {
					return txErr
				}
				if err := revokeRoomScopedTokensForRoom(ctx, tx, updated, now); err != nil {
					return err
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
				if err := s.enqueueWebhookOutboxForEvent(ctx, tx, updated, ev, ""); err != nil {
					return err
				}
				if err := s.enqueueAgentStreamTerminal(ctx, tx, updated, "room.closed", "ttl_expired", eventTime(ev, now)); err != nil {
					return err
				}
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
				if roomLocker, ok := tx.(repository.RoomLockStore); ok {
					if err := roomLocker.LockRoom(ctx, rm.ID); err != nil {
						return err
					}
				}
				current, currentErr := tx.GetRoom(ctx, rm.ID)
				if currentErr != nil {
					return currentErr
				}
				if current.State == domain.RoomStatePurged {
					purgedRoom = current
					return nil
				}
				if current.State != domain.RoomStateClosed {
					return nil
				}
				if txErr := tx.PurgeRoomContent(ctx, rm.ID, now); txErr != nil {
					return txErr
				}
				if err := revokeRoomScopedTokensForRoom(ctx, tx, rm, now); err != nil {
					return err
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
				if getErr != nil {
					return getErr
				}
				if err := s.enqueueWebhookOutboxForEvent(ctx, tx, purgedRoom, ev, ""); err != nil {
					return err
				}
				purgeReason := "retention_cap_reached"
				if activeCount == 0 && pastGrace {
					purgeReason = "grace_elapsed"
				}
				if err := s.enqueueAgentStreamTerminal(ctx, tx, purgedRoom, "room.purged", purgeReason, eventTime(ev, now)); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				return repository.Room{}, err
			}
			s.publishRoomEvents(emitted)
			s.appendAuditEventBestEffort(ctx, rm.ID, "room_purged", map[string]any{
				"detail": "content hard-deleted",
			}, len(msgs))
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

func buildJoinedMap(rm repository.Room, joined map[string]bool) map[string]bool {
	out := map[string]bool{}
	if strings.TrimSpace(rm.AgentAID) != "" {
		out[rm.AgentAID] = roomParticipantJoined(rm, rm.AgentAID, joined)
	}
	if strings.TrimSpace(rm.AgentBID) != "" {
		out[rm.AgentBID] = roomParticipantJoined(rm, rm.AgentBID, joined)
	}
	return out
}

func roomParticipantJoined(rm repository.Room, agentID string, joined map[string]bool) bool {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false
	}
	if joined != nil && joined[agentID] {
		return true
	}
	if strings.TrimSpace(rm.AgentBID) == "" {
		return rm.State == domain.RoomStateOpen && agentID == rm.AgentAID
	}
	return rm.State == domain.RoomStateActive && (agentID == rm.AgentAID || agentID == rm.AgentBID)
}

func nextActorIDForRoom(rm repository.Room) string {
	if rm.State != domain.RoomStateOpen && rm.State != domain.RoomStateActive {
		return ""
	}
	if rm.TurnIndex%2 == 0 {
		return rm.AgentAID
	}
	return rm.AgentBID
}

func roomScopedTokenAllows(scope, action string) bool {
	switch strings.TrimSpace(scope) {
	case roomScopeAutomation:
		return true
	case roomScopeReadOnly:
		switch strings.TrimSpace(action) {
		case "room:state", "room:context":
			return true
		}
	}
	return false
}

func revokeRoomScopedTokensForRoom(ctx context.Context, tx repository.TxStore, rm repository.Room, revokedAt time.Time) error {
	agentIDs := []string{strings.TrimSpace(rm.AgentAID), strings.TrimSpace(rm.AgentBID)}
	for _, agentID := range agentIDs {
		if agentID == "" {
			continue
		}
		if err := tx.RevokeRoomScopedTokens(ctx, rm.ID, agentID, revokedAt); err != nil {
			return err
		}
	}
	return nil
}

func supportsMultiInstanceCoordination(store repository.Store) bool {
	_, ok := store.(repository.MultiInstanceCoordinationStore)
	return ok
}

func lockSharedMessageCoordination(ctx context.Context, tx repository.TxStore, roomID, agentID string) error {
	if roomLocker, ok := tx.(repository.RoomLockStore); ok {
		if err := roomLocker.LockRoom(ctx, roomID); err != nil {
			return err
		}
	}
	if advisoryLocker, ok := tx.(repository.AdvisoryLockStore); ok {
		if err := advisoryLocker.LockAdvisory(ctx, "agent:"+strings.TrimSpace(agentID)); err != nil {
			return err
		}
	}
	return nil
}

func normalizeWebhookEndpointURL(raw string) (string, error) {
	parsed, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("missing scheme or host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("user info, query, and fragment are not allowed")
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	host := strings.TrimSpace(parsed.Hostname())
	switch scheme {
	case "https":
	case "http":
		if host != "localhost" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return "", errors.New("http webhooks are only allowed on loopback hosts")
			}
		}
	default:
		return "", errors.New("unsupported scheme")
	}
	parsed.Scheme = scheme
	return parsed.String(), nil
}

func (s *Service) roomContextFromRoom(rm repository.Room, lastActorID string) roomContextPayload {
	out := roomContextPayload{
		RoomID:                      rm.ID,
		Topic:                       strings.TrimSpace(rm.Topic),
		ConversationMode:            inferConversationMode(rm.Topic),
		AgentAID:                    rm.AgentAID,
		AgentBID:                    rm.AgentBID,
		LastActorID:                 strings.TrimSpace(lastActorID),
		LastContextFetchTurnByAgent: make(map[string]int),
		State:                       string(rm.State),
		TurnIndex:                   rm.TurnIndex,
		MaxTurns:                    rm.MaxTurns,
		TTLAt:                       rm.TTLAt.Format(time.RFC3339),
		RecentMemory:                []recentMemoryEntry{},
	}
	if rm.ClosedAt != nil {
		closed := rm.ClosedAt.Format(time.RFC3339)
		out.ClosedAt = &closed
	}
	return out
}

func (s *Service) roomContextPayloadForUpdate(ctx context.Context, rm repository.Room, lastActorID string) (roomContextPayload, int, error) {
	payload := s.roomContextFromRoom(rm, lastActorID)
	version := 1
	current, err := s.store.GetRoomContext(ctx, rm.ID)
	if err == nil {
		version = current.Version + 1
		var persisted roomContextPayload
		if unmarshalErr := json.Unmarshal(current.Context, &persisted); unmarshalErr == nil {
			if strings.TrimSpace(lastActorID) == "" && (persisted.LastActorID == rm.AgentAID || persisted.LastActorID == rm.AgentBID) {
				payload.LastActorID = persisted.LastActorID
			}
			payload.LastContextFetchTurnByAgent = cloneIntMap(persisted.LastContextFetchTurnByAgent)
		}
	} else if !errors.Is(err, repository.ErrNotFound) {
		return roomContextPayload{}, 0, err
	}
	if payload.LastContextFetchTurnByAgent == nil {
		payload.LastContextFetchTurnByAgent = make(map[string]int)
	}
	return payload, version, nil
}

func (s *Service) loadRoomForContextUpdate(ctx context.Context, roomID string) (repository.Room, error) {
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
	return rm, nil
}

func (s *Service) updateRoomContext(
	ctx context.Context,
	roomID string,
	lastActorID string,
	mutate func(repository.Room, roomContextPayload) (roomContextPayload, error),
) error {
	var lastErr error
	for attempt := 0; attempt < maxRoomContextUpdateTries; attempt++ {
		rm, err := s.loadRoomForContextUpdate(ctx, roomID)
		if err != nil {
			return err
		}
		payload, version, err := s.roomContextPayloadForUpdate(ctx, rm, lastActorID)
		if err != nil {
			return err
		}
		payload, err = mutate(rm, payload)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal room context: %w", err)
		}
		_, err = s.store.UpsertRoomContext(ctx, repository.UpsertRoomContextInput{
			RoomID:  rm.ID,
			Context: raw,
			Version: version,
		})
		if errors.Is(err, repository.ErrConflict) {
			lastErr = err
			continue
		}
		return err
	}
	if lastErr != nil {
		return lastErr
	}
	return repository.ErrConflict
}

func cloneIntMap(src map[string]int) map[string]int {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]int, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func (s *Service) formatTaskContext(payload roomContextPayload, agentID string) string {
	lines := []string{
		fmt.Sprintf("room_id=%s", payload.RoomID),
		fmt.Sprintf("room_topic=%s", safeTaskContextValue(payload.Topic, "(unset)", 160)),
		fmt.Sprintf("conversation_mode=%s", safeTaskContextValue(payload.ConversationMode, "normal_chat", 32)),
		fmt.Sprintf("conversation_summary=%s", safeTaskContextValue(payload.ConversationSummary, "(none)", 320)),
		"topic_anchor=Stay on the room topic unless the user explicitly changes it.",
		"interaction_anchor=Advance the discussion naturally; avoid empty agreement, empty praise, or paraphrase-only turns.",
		fmt.Sprintf("self_agent_id=%s", agentID),
		fmt.Sprintf("voice_hint=%s", safeTaskContextValue(voiceHintForAgent(agentID), "measured and direct", 80)),
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

func voiceHintForAgent(agentID string) string {
	styles := []string{
		"measured and direct",
		"warm and brisk",
		"calm and analytical",
		"plainspoken and lightly probing",
		"concise and reflective",
		"steady and practical",
	}
	trimmed := strings.TrimSpace(agentID)
	if trimmed == "" {
		return styles[0]
	}
	sum := sha256.Sum256([]byte(trimmed))
	return styles[int(sum[0])%len(styles)]
}

func safeTaskContextValue(value, fallback string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	value = strings.Join(strings.Fields(value), " ")
	if maxRunes > 0 {
		runes := []rune(value)
		if len(runes) > maxRunes {
			value = string(runes[:maxRunes]) + "..."
		}
	}
	return value
}

func inferConversationMode(topic string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(topic)), " "))
	if normalized == "" {
		return "normal_chat"
	}
	incidentSignals := []string{
		"incident review",
		"incident-review",
		"incident",
		"triage",
		"postmortem",
		"post-mortem",
		"mortem",
		"runbook",
		"ops review",
		"outage",
		"sev",
		"pager",
		"alert",
		"rca",
		"root cause",
	}
	for _, signal := range incidentSignals {
		if topicSignalMatches(normalized, signal) {
			return "incident_review"
		}
	}
	return "normal_chat"
}

func topicSignalMatches(normalized, signal string) bool {
	signal = strings.TrimSpace(strings.ToLower(signal))
	if signal == "" {
		return false
	}
	if strings.Contains(signal, " ") || strings.Contains(signal, "-") {
		return strings.Contains(normalized, signal)
	}
	for _, token := range strings.Fields(normalized) {
		if token == signal {
			return true
		}
		if signal == "sev" && strings.HasPrefix(token, "sev") {
			return true
		}
	}
	return false
}

func buildConversationSummary(topic, mode string, recent []repository.Message) string {
	topic = safeTaskContextValue(topic, "(unset)", 80)
	mode = safeTaskContextValue(mode, "normal_chat", 32)
	if len(recent) == 0 {
		return fmt.Sprintf("topic=%s | mode=%s | recent=none", topic, mode)
	}
	start := len(recent) - 3
	if start < 0 {
		start = 0
	}
	parts := make([]string, 0, len(recent)-start)
	for _, m := range recent[start:] {
		parts = append(parts, fmt.Sprintf("%s:%s", safeTaskContextValue(m.SenderID, "unknown", 24), safeTaskContextValue(m.Ciphertext, "(empty)", 80)))
	}
	return fmt.Sprintf("topic=%s | mode=%s | last_turn=%d | recent=%s", topic, mode, recent[len(recent)-1].Turn, strings.Join(parts, "; "))
}

func (s *Service) upsertRoomContext(ctx context.Context, rm repository.Room, lastActorID string) error {
	return s.updateRoomContext(ctx, rm.ID, lastActorID, func(current repository.Room, payload roomContextPayload) (roomContextPayload, error) {
		recent, err := s.store.ListRoomMessages(ctx, current.ID)
		if err != nil {
			return roomContextPayload{}, err
		}
		recent, err = s.decryptRoomMessages(current, recent)
		if err != nil {
			return roomContextPayload{}, err
		}
		payload.ConversationSummary = buildConversationSummary(payload.Topic, payload.ConversationMode, recent)
		payload.RecentMemory = selectRecentMemory(recent)
		return payload, nil
	})
}

func (s *Service) syncRoomContextBestEffort(ctx context.Context, rm repository.Room, source string, recentCount int) {
	if err := s.upsertRoomContext(ctx, rm, ""); err != nil {
		s.appendAuditEventBestEffort(ctx, rm.ID, "room_context_sync_failed", map[string]any{
			"room_id": rm.ID,
			"reason":  err.Error(),
			"source":  source,
		}, recentCount)
	}
}

func (s *Service) enqueueAgentStreamTurnReady(ctx context.Context, tx repository.TxStore, rm repository.Room, reason string, occurredAt time.Time) error {
	targetAgentID := strings.TrimSpace(nextActorIDForRoom(rm))
	if rm.State != domain.RoomStateActive || targetAgentID == "" {
		return nil
	}
	return s.createAgentStreamDelivery(ctx, tx, repository.CreateAgentStreamDeliveryInput{
		DeliveryID: newID("dly"),
		AgentID:    targetAgentID,
		RoomID:     rm.ID,
		Type:       "room.turn_ready",
		Reason:     strings.TrimSpace(reason),
		Payload: mustMarshalJSON(map[string]any{
			"type":            "room.turn_ready",
			"room_id":         rm.ID,
			"target_agent_id": targetAgentID,
			"room_state":      string(rm.State),
			"next_turn":       rm.TurnIndex,
			"next_actor_id":   targetAgentID,
			"reason":          strings.TrimSpace(reason),
			"occurred_at":     occurredAt.Format(time.RFC3339Nano),
		}),
		Status:    "pending",
		ExpiresAt: occurredAt.Add(agentStreamReplayWindow),
	})
}

func (s *Service) enqueueAgentStreamTerminal(ctx context.Context, tx repository.TxStore, rm repository.Room, typ, reason string, occurredAt time.Time) error {
	for _, targetAgentID := range uniqueAgentIDs(rm.AgentAID, rm.AgentBID) {
		payload := map[string]any{
			"type":            typ,
			"room_id":         rm.ID,
			"target_agent_id": targetAgentID,
			"room_state":      string(rm.State),
			"reason":          strings.TrimSpace(reason),
			"occurred_at":     occurredAt.Format(time.RFC3339Nano),
		}
		if rm.ClosedAt != nil {
			payload["closed_at"] = rm.ClosedAt.Format(time.RFC3339Nano)
		}
		if rm.PurgedAt != nil {
			payload["purged_at"] = rm.PurgedAt.Format(time.RFC3339Nano)
		}
		if err := s.createAgentStreamDelivery(ctx, tx, repository.CreateAgentStreamDeliveryInput{
			DeliveryID: newID("dly"),
			AgentID:    targetAgentID,
			RoomID:     rm.ID,
			Type:       typ,
			Reason:     strings.TrimSpace(reason),
			Payload:    mustMarshalJSON(payload),
			Status:     "pending",
			ExpiresAt:  occurredAt.Add(agentStreamReplayWindow),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) createAgentStreamDelivery(ctx context.Context, tx repository.TxStore, in repository.CreateAgentStreamDeliveryInput) error {
	_, err := tx.CreateAgentStreamDelivery(ctx, in)
	return err
}

func (s *Service) enqueueWebhookOutboxForEvent(ctx context.Context, tx repository.TxStore, rm repository.Room, event repository.RoomEvent, actorID string) error {
	if !supportsWebhookOutbox(event.EventType) {
		return nil
	}

	targetAgentIDs := webhookTargetAgentIDs(rm, actorID)
	if len(targetAgentIDs) == 0 {
		return nil
	}

	payloadReason := webhookEventReason(event.EventType)
	nextActorID := nextActorIDForRoom(rm)
	nextTurn := rm.TurnIndex
	attemptAt := s.now()

	for _, targetAgentID := range targetAgentIDs {
		endpoints, err := tx.ListAgentWebhookEndpoints(ctx, targetAgentID)
		if err != nil {
			return err
		}
		if len(endpoints) == 0 {
			continue
		}

		payload := map[string]any{
			"event_id":        event.ID,
			"type":            event.EventType,
			"room_id":         rm.ID,
			"target_agent_id": targetAgentID,
			"room_state":      string(rm.State),
			"next_turn":       nextTurn,
			"reason":          payloadReason,
			"occurred_at":     event.CreatedAt.Format(time.RFC3339Nano),
		}
		if nextActorID != "" {
			payload["next_actor_id"] = nextActorID
		}
		if otherAgentID := otherAgentIDForWebhook(rm, targetAgentID); otherAgentID != "" {
			payload["other_agent_id"] = otherAgentID
		}

		for _, endpoint := range endpoints {
			if !endpoint.Enabled {
				continue
			}
			payload["delivery_id"] = newID("whd")
			rawPayload, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("marshal webhook outbox payload: %w", err)
			}
			if _, err := tx.CreateWebhookOutbox(ctx, repository.CreateWebhookOutboxInput{
				RoomID:        rm.ID,
				RoomEventID:   event.ID,
				TargetAgentID: targetAgentID,
				EndpointID:    endpoint.ID,
				EventType:     event.EventType,
				Payload:       rawPayload,
				Status:        "pending",
				NextAttemptAt: attemptAt,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func supportsWebhookOutbox(eventType string) bool {
	switch eventType {
	case "room.joined", "message.created", "room.closed", "room.purged":
		return true
	default:
		return false
	}
}

func webhookEventReason(eventType string) string {
	return strings.ReplaceAll(strings.TrimSpace(eventType), ".", "_")
}

func eventTime(ev repository.RoomEvent, fallback time.Time) time.Time {
	if !ev.CreatedAt.IsZero() {
		return ev.CreatedAt
	}
	return fallback
}

func uniqueAgentIDs(ids ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func mustMarshalJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func webhookTargetAgentIDs(rm repository.Room, actorID string) []string {
	actorID = strings.TrimSpace(actorID)
	candidates := []string{strings.TrimSpace(rm.AgentAID), strings.TrimSpace(rm.AgentBID)}
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" || candidate == actorID || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

func otherAgentIDForWebhook(rm repository.Room, targetAgentID string) string {
	targetAgentID = strings.TrimSpace(targetAgentID)
	if targetAgentID == "" {
		return ""
	}
	if targetAgentID == strings.TrimSpace(rm.AgentAID) {
		return strings.TrimSpace(rm.AgentBID)
	}
	if targetAgentID == strings.TrimSpace(rm.AgentBID) {
		return strings.TrimSpace(rm.AgentAID)
	}
	return ""
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

func (s *Service) newRoomMessageCipher() (string, *secretcipher.Cipher, error) {
	keyMaterial := "mk_" + randomToken(32)
	wrapped, err := s.roomSeal.Encrypt(keyMaterial)
	if err != nil {
		return "", nil, err
	}
	return wrapped, secretcipher.New(keyMaterial), nil
}

func (s *Service) roomMessageCipher(rm repository.Room) (*secretcipher.Cipher, error) {
	if strings.TrimSpace(rm.MessageKeyCiphertext) == "" {
		return nil, nil
	}
	keyMaterial, err := s.roomSeal.Decrypt(rm.MessageKeyCiphertext)
	if err != nil {
		return nil, err
	}
	return secretcipher.New(keyMaterial), nil
}

func (s *Service) ensureRoomMessageCipher(ctx context.Context, tx repository.TxStore, rm repository.Room) (repository.Room, *secretcipher.Cipher, error) {
	if strings.TrimSpace(rm.MessageKeyCiphertext) != "" {
		cipher, err := s.roomMessageCipher(rm)
		return rm, cipher, err
	}
	wrapped, cipher, err := s.newRoomMessageCipher()
	if err != nil {
		return repository.Room{}, nil, err
	}
	updated, err := tx.UpdateRoom(ctx, repository.UpdateRoomInput{
		ID:                   rm.ID,
		MessageKeyCiphertext: &wrapped,
	})
	if err != nil {
		return repository.Room{}, nil, err
	}
	return updated, cipher, nil
}

func (s *Service) decryptRoomMessages(rm repository.Room, msgs []repository.Message) ([]repository.Message, error) {
	cipher, err := s.roomMessageCipher(rm)
	if err != nil || cipher == nil {
		return msgs, err
	}
	out := make([]repository.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if strings.TrimSpace(out[i].Ciphertext) == "" {
			continue
		}
		plain, decErr := cipher.Decrypt(out[i].Ciphertext)
		if decErr != nil {
			return nil, decErr
		}
		out[i].Ciphertext = plain
	}
	return out, nil
}

func (s *Service) decryptRoomEvents(rm repository.Room, items []repository.RoomEvent) ([]repository.RoomEvent, error) {
	cipher, err := s.roomMessageCipher(rm)
	if err != nil || cipher == nil {
		return items, err
	}
	out := make([]repository.RoomEvent, len(items))
	copy(out, items)
	for i := range out {
		if out[i].Ciphertext == nil || strings.TrimSpace(*out[i].Ciphertext) == "" {
			continue
		}
		plain, decErr := cipher.Decrypt(*out[i].Ciphertext)
		if decErr != nil {
			return nil, decErr
		}
		out[i].Ciphertext = &plain
	}
	return out, nil
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
