package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/febrian/areyouai/internal/repository"
	"github.com/febrian/areyouai/internal/security/secretcipher"
	"github.com/febrian/areyouai/internal/security/webhooksigner"
)

type Store interface {
	ClaimPendingWebhookDeliveries(ctx context.Context, now, reclaimBefore time.Time, limit int) ([]repository.ClaimedWebhookDelivery, error)
	MarkWebhookOutboxDelivered(ctx context.Context, id int64) error
	MarkWebhookOutboxPendingRetry(ctx context.Context, id int64, nextAttemptAt time.Time, lastError string) error
	MarkWebhookOutboxDeadLetter(ctx context.Context, id int64, lastError string) error
}

type Config struct {
	PollInterval    time.Duration
	BatchSize       int
	DeliveryTimeout time.Duration
	ClaimStaleAfter time.Duration
	MaxAttempts     int
	BaseBackoff     time.Duration
	MaxBackoff      time.Duration
	SecretKey       string
	SecretKeyset    string
	Now             func() time.Time
	HTTPClient      *http.Client
}

type Worker struct {
	store           Store
	client          *http.Client
	now             func() time.Time
	pollInterval    time.Duration
	batchSize       int
	deliveryTimeout time.Duration
	claimStaleAfter time.Duration
	maxAttempts     int
	baseBackoff     time.Duration
	maxBackoff      time.Duration
	seal            *secretcipher.Cipher

	randMu sync.Mutex
	rnd    *rand.Rand
}

func New(store Store, cfg Config) *Worker {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > 100 {
		cfg.BatchSize = 20
	}
	if cfg.DeliveryTimeout <= 0 {
		cfg.DeliveryTimeout = 10 * time.Second
	}
	if cfg.ClaimStaleAfter <= 0 {
		cfg.ClaimStaleAfter = 45 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 5 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	return &Worker{
		store:           store,
		client:          cfg.HTTPClient,
		now:             cfg.Now,
		pollInterval:    cfg.PollInterval,
		batchSize:       cfg.BatchSize,
		deliveryTimeout: cfg.DeliveryTimeout,
		claimStaleAfter: cfg.ClaimStaleAfter,
		maxAttempts:     cfg.MaxAttempts,
		baseBackoff:     cfg.BaseBackoff,
		maxBackoff:      cfg.MaxBackoff,
		seal:            secretcipher.NewWithKeyset(cfg.SecretKey, cfg.SecretKeyset),
		rnd:             rand.New(rand.NewSource(cfg.Now().UnixNano())),
	}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		if _, err := w.RunOnce(ctx); err != nil {
			log.Printf("webhook_worker_run_once_failed err=%v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	now := w.now()
	deliveries, err := w.store.ClaimPendingWebhookDeliveries(ctx, now, now.Add(-w.claimStaleAfter), w.batchSize)
	if err != nil {
		return 0, err
	}
	for _, delivery := range deliveries {
		w.processDelivery(ctx, delivery)
	}
	return len(deliveries), nil
}

func (w *Worker) processDelivery(ctx context.Context, delivery repository.ClaimedWebhookDelivery) {
	if !delivery.EndpointEnabled {
		w.markDeadLetter(ctx, delivery, "endpoint disabled")
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, w.deliveryTimeout)
	defer cancel()

	req, err := w.buildRequest(reqCtx, delivery)
	if err != nil {
		w.failDelivery(ctx, delivery, err, 0)
		return
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.failDelivery(ctx, delivery, err, 0)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := w.store.MarkWebhookOutboxDelivered(ctx, delivery.ID); err != nil {
			log.Printf("webhook_delivery_mark_delivered_failed delivery_id=%d err=%v", delivery.ID, err)
			return
		}
		log.Printf(
			"webhook_delivery_delivered outbox_id=%d room_id=%s event_type=%s target_agent_id=%s endpoint_host=%s attempt=%d status=%d",
			delivery.ID,
			delivery.RoomID,
			delivery.EventType,
			delivery.TargetAgentID,
			endpointHost(delivery.EndpointURL),
			delivery.AttemptCount,
			resp.StatusCode,
		)
		return
	}

	w.failDelivery(ctx, delivery, fmt.Errorf("remote status %d", resp.StatusCode), resp.StatusCode)
}

func (w *Worker) buildRequest(ctx context.Context, delivery repository.ClaimedWebhookDelivery) (*http.Request, error) {
	body := []byte(delivery.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.EndpointURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	secret, err := w.seal.Decrypt(delivery.EndpointSecretCiphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	headers := webhooksigner.Sign(secret, body, w.now())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "areyouai-webhook-worker/1.0")
	req.Header.Set("X-Areyouai-Timestamp", headers.Timestamp)
	req.Header.Set("X-Areyouai-Signature", headers.Signature)
	req.Header.Set("X-Areyouai-Key-Id", delivery.EndpointKeyID)
	req.Header.Set("X-Areyouai-Event-Type", delivery.EventType)
	if deliveryID := webhookPayloadString(delivery.Payload, "delivery_id"); deliveryID != "" {
		req.Header.Set("X-Areyouai-Delivery-Id", deliveryID)
	}
	return req, nil
}

func (w *Worker) failDelivery(ctx context.Context, delivery repository.ClaimedWebhookDelivery, err error, statusCode int) {
	lastError := clipError(err, statusCode)
	if statusCode >= 400 && statusCode < 500 && statusCode != http.StatusTooManyRequests && statusCode != http.StatusRequestTimeout {
		w.markDeadLetter(ctx, delivery, lastError)
		return
	}
	if delivery.AttemptCount >= w.maxAttempts {
		w.markDeadLetter(ctx, delivery, lastError)
		return
	}

	nextAttemptAt := w.nextRetryTime(delivery.AttemptCount)
	if updateErr := w.store.MarkWebhookOutboxPendingRetry(ctx, delivery.ID, nextAttemptAt, lastError); updateErr != nil {
		log.Printf("webhook_delivery_mark_retry_failed outbox_id=%d err=%v", delivery.ID, updateErr)
		return
	}
	log.Printf(
		"webhook_delivery_retry_scheduled outbox_id=%d room_id=%s event_type=%s target_agent_id=%s endpoint_host=%s attempt=%d next_attempt_at=%s err=%s",
		delivery.ID,
		delivery.RoomID,
		delivery.EventType,
		delivery.TargetAgentID,
		endpointHost(delivery.EndpointURL),
		delivery.AttemptCount,
		nextAttemptAt.Format(time.RFC3339Nano),
		lastError,
	)
}

func (w *Worker) markDeadLetter(ctx context.Context, delivery repository.ClaimedWebhookDelivery, lastError string) {
	if err := w.store.MarkWebhookOutboxDeadLetter(ctx, delivery.ID, lastError); err != nil {
		log.Printf("webhook_delivery_mark_dead_letter_failed outbox_id=%d err=%v", delivery.ID, err)
		return
	}
	log.Printf(
		"webhook_delivery_dead_letter outbox_id=%d room_id=%s event_type=%s target_agent_id=%s endpoint_host=%s attempt=%d err=%s",
		delivery.ID,
		delivery.RoomID,
		delivery.EventType,
		delivery.TargetAgentID,
		endpointHost(delivery.EndpointURL),
		delivery.AttemptCount,
		lastError,
	)
}

func (w *Worker) nextRetryTime(attempt int) time.Time {
	backoff := w.baseBackoff
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff >= w.maxBackoff {
			backoff = w.maxBackoff
			break
		}
	}
	jitter := w.randomJitter(backoff / 4)
	return w.now().Add(backoff + jitter)
}

func (w *Worker) randomJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	w.randMu.Lock()
	defer w.randMu.Unlock()
	return time.Duration(w.rnd.Int63n(int64(max)))
}

func webhookPayloadString(payload json.RawMessage, key string) string {
	if len(payload) == 0 {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	value, _ := decoded[key].(string)
	return strings.TrimSpace(value)
}

func endpointHost(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "invalid"
	}
	host := strings.TrimSpace(parsed.Host)
	if host == "" {
		return "invalid"
	}
	return host
}

func clipError(err error, statusCode int) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if statusCode > 0 {
		if msg != "" {
			msg = fmt.Sprintf("%s (status=%d)", msg, statusCode)
		} else {
			msg = fmt.Sprintf("status=%d", statusCode)
		}
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 512 {
		return msg[:512]
	}
	return msg
}
