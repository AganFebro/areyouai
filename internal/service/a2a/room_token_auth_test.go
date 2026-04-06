package a2a

import (
	"context"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/repository"
)

type roomTokenAuthStore struct {
	repository.Store

	token         repository.RoomScopedToken
	touchCalls    int
	lastUsedAt    time.Time
	lastExpiresAt time.Time
}

func (s *roomTokenAuthStore) FindSession(ctx context.Context, token string) (repository.Session, error) {
	return repository.Session{}, repository.ErrNotFound
}

func (s *roomTokenAuthStore) FindRoomScopedTokenByHash(ctx context.Context, tokenHash string) (repository.RoomScopedToken, error) {
	if tokenHash != s.token.TokenHash {
		return repository.RoomScopedToken{}, repository.ErrNotFound
	}
	return s.token, nil
}

func (s *roomTokenAuthStore) TouchRoomScopedToken(ctx context.Context, tokenHash string, lastUsedAt, expiresAt time.Time) error {
	if tokenHash != s.token.TokenHash {
		return repository.ErrNotFound
	}
	s.touchCalls++
	s.lastUsedAt = lastUsedAt
	s.lastExpiresAt = expiresAt
	s.token.ExpiresAt = expiresAt
	return nil
}

func TestAuthRoomAccessDoesNotRefreshFreshRoomTokenOnReads(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0).UTC()
	store := &roomTokenAuthStore{
		token: repository.RoomScopedToken{
			RoomID:    "room_1",
			AgentID:   "agt_a",
			TokenHash: hashText("rat_1"),
			Scope:     roomScopeReadOnly,
			ExpiresAt: now.Add(4 * time.Minute),
		},
	}
	svc := New(store, Options{})
	svc.now = func() time.Time { return now }

	agentID, err := svc.AuthRoomAccess(context.Background(), "rat_1", "room_1", "room:state")
	if err != nil {
		t.Fatalf("AuthRoomAccess() error = %v", err)
	}
	if agentID != "agt_a" {
		t.Fatalf("agentID=%q want=%q", agentID, "agt_a")
	}
	if store.touchCalls != 0 {
		t.Fatalf("touchCalls=%d want=0", store.touchCalls)
	}

	agentID, err = svc.AuthRoomAccess(context.Background(), "rat_1", "room_1", "room:context")
	if err != nil {
		t.Fatalf("AuthRoomAccess(context) error = %v", err)
	}
	if agentID != "agt_a" {
		t.Fatalf("agentID=%q want=%q", agentID, "agt_a")
	}
	if store.touchCalls != 0 {
		t.Fatalf("touchCalls=%d want=0 after context read", store.touchCalls)
	}
}

func TestAuthRoomAccessRefreshesNearExpiryRoomToken(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0).UTC()
	store := &roomTokenAuthStore{
		token: repository.RoomScopedToken{
			RoomID:    "room_1",
			AgentID:   "agt_a",
			TokenHash: hashText("rat_1"),
			Scope:     roomScopeReadOnly,
			ExpiresAt: now.Add(30 * time.Second),
		},
	}
	svc := New(store, Options{})
	svc.now = func() time.Time { return now }

	if _, err := svc.AuthRoomAccess(context.Background(), "rat_1", "room_1", "room:state"); err != nil {
		t.Fatalf("AuthRoomAccess() error = %v", err)
	}
	if store.touchCalls != 1 {
		t.Fatalf("touchCalls=%d want=1", store.touchCalls)
	}
	if !store.lastExpiresAt.After(now.Add(roomScopedTokenTTL - time.Second)) {
		t.Fatalf("expected refreshed expiry, got %s", store.lastExpiresAt)
	}
}
