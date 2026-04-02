package webhooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/repository"
)

type fakeStore struct {
	claimed           []repository.ClaimedWebhookDelivery
	markedDelivered   []int64
	markedRetry       []retryMark
	markedDeadLetters []deadLetterMark
}

type retryMark struct {
	id            int64
	nextAttemptAt time.Time
	lastError     string
}

type deadLetterMark struct {
	id        int64
	lastError string
}

func (s *fakeStore) ClaimPendingWebhookDeliveries(context.Context, time.Time, time.Time, int) ([]repository.ClaimedWebhookDelivery, error) {
	out := s.claimed
	s.claimed = nil
	return out, nil
}

func (s *fakeStore) MarkWebhookOutboxDelivered(_ context.Context, id int64) error {
	s.markedDelivered = append(s.markedDelivered, id)
	return nil
}

func (s *fakeStore) MarkWebhookOutboxPendingRetry(_ context.Context, id int64, nextAttemptAt time.Time, lastError string) error {
	s.markedRetry = append(s.markedRetry, retryMark{id: id, nextAttemptAt: nextAttemptAt, lastError: lastError})
	return nil
}

func (s *fakeStore) MarkWebhookOutboxDeadLetter(_ context.Context, id int64, lastError string) error {
	s.markedDeadLetters = append(s.markedDeadLetters, deadLetterMark{id: id, lastError: lastError})
	return nil
}

func TestWorkerRunOnceDeliversWebhook(t *testing.T) {
	t.Parallel()

	var gotSignature string
	var gotDeliveryID string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get("X-Areyouai-Signature")
		gotDeliveryID = r.Header.Get("X-Areyouai-Delivery-Id")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	store := &fakeStore{
		claimed: []repository.ClaimedWebhookDelivery{
			{
				WebhookOutboxItem: repository.WebhookOutboxItem{
					ID:            11,
					RoomID:        "room_1",
					RoomEventID:   101,
					TargetAgentID: "agt_b",
					EndpointID:    "ep_1",
					EventType:     "message.created",
					Payload:       json.RawMessage(`{"delivery_id":"whd_1","room_id":"room_1"}`),
					AttemptCount:  1,
				},
				EndpointURL:              server.URL,
				EndpointSecretCiphertext: "secret",
				EndpointKeyID:            "kid_1",
				EndpointEnabled:          true,
			},
		},
	}

	worker := New(store, Config{
		Now: func() time.Time { return time.Unix(1712016000, 0).UTC() },
	})
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	if len(store.markedDelivered) != 1 || store.markedDelivered[0] != 11 {
		t.Fatalf("marked delivered=%v want=[11]", store.markedDelivered)
	}
	if gotDeliveryID != "whd_1" {
		t.Fatalf("delivery header=%q want=whd_1", gotDeliveryID)
	}
	if gotSignature == "" {
		t.Fatal("expected signature header")
	}
	if gotRoomID, _ := gotBody["room_id"].(string); gotRoomID != "room_1" {
		t.Fatalf("body room_id=%v want=room_1", gotBody["room_id"])
	}
}

func TestWorkerRunOnceSchedulesRetryOnServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	now := time.Unix(1712016000, 0).UTC()
	store := &fakeStore{
		claimed: []repository.ClaimedWebhookDelivery{
			{
				WebhookOutboxItem: repository.WebhookOutboxItem{
					ID:            12,
					RoomID:        "room_2",
					TargetAgentID: "agt_b",
					EndpointID:    "ep_2",
					EventType:     "room.closed",
					Payload:       json.RawMessage(`{"delivery_id":"whd_2"}`),
					AttemptCount:  2,
				},
				EndpointURL:              server.URL,
				EndpointSecretCiphertext: "secret",
				EndpointKeyID:            "kid_2",
				EndpointEnabled:          true,
			},
		},
	}

	worker := New(store, Config{
		Now:         func() time.Time { return now },
		BaseBackoff: 5 * time.Second,
		MaxBackoff:  30 * time.Second,
	})
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	if len(store.markedRetry) != 1 {
		t.Fatalf("marked retry len=%d want=1", len(store.markedRetry))
	}
	if store.markedRetry[0].id != 12 {
		t.Fatalf("retry id=%d want=12", store.markedRetry[0].id)
	}
	if !store.markedRetry[0].nextAttemptAt.After(now) {
		t.Fatalf("next attempt=%s want after %s", store.markedRetry[0].nextAttemptAt, now)
	}
	if len(store.markedDeadLetters) != 0 {
		t.Fatalf("unexpected dead letters=%v", store.markedDeadLetters)
	}
}

func TestWorkerRunOnceDeadLettersOnClientError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	store := &fakeStore{
		claimed: []repository.ClaimedWebhookDelivery{
			{
				WebhookOutboxItem: repository.WebhookOutboxItem{
					ID:            13,
					RoomID:        "room_3",
					TargetAgentID: "agt_b",
					EndpointID:    "ep_3",
					EventType:     "room.joined",
					Payload:       json.RawMessage(`{"delivery_id":"whd_3"}`),
					AttemptCount:  1,
				},
				EndpointURL:              server.URL,
				EndpointSecretCiphertext: "secret",
				EndpointKeyID:            "kid_3",
				EndpointEnabled:          true,
			},
		},
	}

	worker := New(store, Config{
		Now: func() time.Time { return time.Unix(1712016000, 0).UTC() },
	})
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	if len(store.markedDeadLetters) != 1 || store.markedDeadLetters[0].id != 13 {
		t.Fatalf("dead letters=%v want one for 13", store.markedDeadLetters)
	}
	if len(store.markedRetry) != 0 {
		t.Fatalf("unexpected retries=%v", store.markedRetry)
	}
}

func TestWorkerRunOnceDeadLettersDisabledEndpoint(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		claimed: []repository.ClaimedWebhookDelivery{
			{
				WebhookOutboxItem: repository.WebhookOutboxItem{
					ID:            14,
					RoomID:        "room_4",
					TargetAgentID: "agt_b",
					EndpointID:    "ep_4",
					EventType:     "message.created",
					Payload:       json.RawMessage(`{"delivery_id":"whd_4"}`),
					AttemptCount:  1,
				},
				EndpointURL:              "https://example.com/hooks",
				EndpointSecretCiphertext: "secret",
				EndpointKeyID:            "kid_4",
				EndpointEnabled:          false,
			},
		},
	}

	worker := New(store, Config{
		Now: func() time.Time { return time.Unix(1712016000, 0).UTC() },
	})
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	if len(store.markedDeadLetters) != 1 || store.markedDeadLetters[0].id != 14 {
		t.Fatalf("dead letters=%v want one for 14", store.markedDeadLetters)
	}
}
