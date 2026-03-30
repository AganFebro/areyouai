package a2a

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/febrian/areyouai/internal/domain"
	"github.com/febrian/areyouai/internal/repository"
)

const maxMessagesPerMinute = 30

var (
	ErrBadRequest   = errors.New("bad request")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrGone         = errors.New("gone")
	ErrRateLimit    = errors.New("rate limit")
)

type Service struct {
	store repository.Store

	mu             sync.Mutex
	joined         map[string]map[string]bool
	messageWindows map[string][]time.Time

	now                    func() time.Time
	viewerHeartbeatTimeout time.Duration
	closedRoomGraceDelay   time.Duration
	maxClosedRetention     time.Duration
}

type Options struct {
	ViewerHeartbeatTimeout time.Duration
	ClosedRoomGraceDelay   time.Duration
	MaxClosedRetention     time.Duration
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
	s := &Service{
		store:                  store,
		joined:                 make(map[string]map[string]bool),
		messageWindows:         make(map[string][]time.Time),
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
	_, err = s.store.CreateSession(ctx, repository.CreateSessionInput{
		Token:   token,
		AgentID: agent.ID,
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
		_, err := s.store.UpdateRoom(ctx, repository.UpdateRoomInput{
			ID:    rm.ID,
			State: &next,
		})
		if err != nil {
			return "", nil, err
		}
		return next, joined, nil
	}
	return rm.State, joined, nil
}

func (s *Service) SendMessage(ctx context.Context, agentID, roomID string, expectedTurn int, ciphertext string) (repository.Message, domain.RoomState, int, error) {
	ciphertext = strings.TrimSpace(ciphertext)
	if ciphertext == "" {
		return repository.Message{}, "", 0, ErrBadRequest
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rm, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return repository.Message{}, "", 0, ErrNotFound
		}
		return repository.Message{}, "", 0, err
	}
	rm, err = s.reconcileRoomLocked(ctx, rm)
	if err != nil {
		return repository.Message{}, "", 0, err
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		return repository.Message{}, "", 0, ErrForbidden
	}
	if rm.State == domain.RoomStateClosed || rm.State == domain.RoomStatePurged {
		return repository.Message{}, "", 0, ErrGone
	}
	if s.now().After(rm.TTLAt) {
		return repository.Message{}, "", 0, ErrGone
	}
	if !s.allowMessageLocked(agentID, s.now()) {
		return repository.Message{}, "", 0, ErrRateLimit
	}
	if expectedTurn != rm.TurnIndex {
		return repository.Message{}, "", 0, ErrConflict
	}

	expectedSender := rm.AgentAID
	if rm.TurnIndex%2 == 1 {
		expectedSender = rm.AgentBID
	}
	if expectedSender != agentID {
		return repository.Message{}, "", 0, ErrConflict
	}

	msg, err := s.store.AppendMessage(ctx, repository.AppendMessageInput{
		ID:         newID("msg"),
		RoomID:     roomID,
		SenderID:   agentID,
		Turn:       rm.TurnIndex,
		Ciphertext: ciphertext,
	})
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return repository.Message{}, "", 0, ErrConflict
		}
		return repository.Message{}, "", 0, err
	}

	nextTurn := rm.TurnIndex + 1
	update := repository.UpdateRoomInput{
		ID:        rm.ID,
		TurnIndex: &nextTurn,
	}
	nextState := rm.State
	if nextTurn >= rm.MaxTurns {
		now := s.now()
		closed := domain.RoomStateClosed
		update.State = &closed
		update.ClosedAt = &now
		nextState = closed
	}
	_, err = s.store.UpdateRoom(ctx, update)
	if err != nil {
		return repository.Message{}, "", 0, err
	}
	return msg, nextState, nextTurn, nil
}

type RoomStateResult struct {
	Room          repository.Room
	ActiveViewers int
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

	now := s.now()
	closed := domain.RoomStateClosed
	return s.store.UpdateRoom(ctx, repository.UpdateRoomInput{
		ID:       roomID,
		State:    &closed,
		ClosedAt: &now,
	})
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
			return s.store.UpdateRoom(ctx, repository.UpdateRoomInput{
				ID:       rm.ID,
				State:    &closed,
				ClosedAt: &now,
			})
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
			if err := s.store.PurgeRoomContent(ctx, rm.ID, now); err != nil {
				return repository.Room{}, err
			}
			if err := s.store.AppendAuditEvent(ctx, repository.AppendAuditEventInput{
				RoomID:       rm.ID,
				Event:        "room_purged",
				Meta:         "content hard-deleted",
				MessageCount: len(msgs),
			}); err != nil {
				return repository.Room{}, err
			}
			return s.store.GetRoom(ctx, rm.ID)
		}
	}
	return rm, nil
}

func (s *Service) allowMessageLocked(agentID string, now time.Time) bool {
	windowStart := now.Add(-1 * time.Minute)
	timestamps := s.messageWindows[agentID]
	kept := timestamps[:0]
	for _, t := range timestamps {
		if t.After(windowStart) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxMessagesPerMinute {
		s.messageWindows[agentID] = kept
		return false
	}
	kept = append(kept, now)
	s.messageWindows[agentID] = kept
	return true
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
