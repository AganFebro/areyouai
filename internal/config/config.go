package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	APIAddr                string
	PostgresDSN            string
	RedisAddr              string
	AdminToken             string
	ViewerHeartbeatTimeout time.Duration
	ClosedRoomGraceDelay   time.Duration
	MaxClosedRetention     time.Duration
	WebhookWorkerEnabled   bool
	WebhookSecretKey       string
	WebhookSecretKeyset    string
	RoomDEKKey             string
	RoomDEKKeyset          string
	WebhookPollInterval    time.Duration
	WebhookDeliveryTimeout time.Duration
	WebhookClaimStaleAfter time.Duration
	WebhookBatchSize       int
	WebhookMaxAttempts     int
	WebhookBaseBackoff     time.Duration
	WebhookMaxBackoff      time.Duration
	PurgeWorkerEnabled     bool
	PurgePollInterval      time.Duration
	PurgeBatchSize         int
}

func Load() Config {
	return Config{
		APIAddr:                getenv("API_ADDR", "127.0.0.1:8080"),
		PostgresDSN:            getenv("POSTGRES_DSN", ""),
		RedisAddr:              getenv("REDIS_ADDR", "localhost:6379"),
		AdminToken:             getenv("ADMIN_TOKEN", ""),
		ViewerHeartbeatTimeout: getDurationSeconds("VIEWER_HEARTBEAT_TIMEOUT_SECONDS", 45),
		ClosedRoomGraceDelay:   getDurationSeconds("CLOSED_ROOM_GRACE_DELAY_SECONDS", 120),
		MaxClosedRetention:     getDurationSeconds("MAX_CLOSED_RETENTION_SECONDS", 86400),
		WebhookWorkerEnabled:   getBool("WEBHOOK_WORKER_ENABLED", true),
		WebhookSecretKey:       getenv("WEBHOOK_SECRET_ENCRYPTION_KEY", ""),
		WebhookSecretKeyset:    getenv("WEBHOOK_SECRET_ENCRYPTION_KEYS", ""),
		RoomDEKKey:             getenv("ROOM_DEK_ENCRYPTION_KEY", ""),
		RoomDEKKeyset:          getenv("ROOM_DEK_ENCRYPTION_KEYS", ""),
		WebhookPollInterval:    getDurationSeconds("WEBHOOK_POLL_INTERVAL_SECONDS", 2),
		WebhookDeliveryTimeout: getDurationSeconds("WEBHOOK_DELIVERY_TIMEOUT_SECONDS", 10),
		WebhookClaimStaleAfter: getDurationSeconds("WEBHOOK_CLAIM_STALE_AFTER_SECONDS", 45),
		WebhookBatchSize:       getInt("WEBHOOK_BATCH_SIZE", 20),
		WebhookMaxAttempts:     getInt("WEBHOOK_MAX_ATTEMPTS", 8),
		WebhookBaseBackoff:     getDurationSeconds("WEBHOOK_BASE_BACKOFF_SECONDS", 5),
		WebhookMaxBackoff:      getDurationSeconds("WEBHOOK_MAX_BACKOFF_SECONDS", 300),
		PurgeWorkerEnabled:     getBool("PURGE_WORKER_ENABLED", true),
		PurgePollInterval:      getDurationSeconds("PURGE_POLL_INTERVAL_SECONDS", 15),
		PurgeBatchSize:         getInt("PURGE_BATCH_SIZE", 200),
	}
}

func getenv(k, fallback string) string {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	return v
}

func getDurationSeconds(name string, fallbackSeconds int) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return time.Duration(fallbackSeconds) * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return time.Duration(fallbackSeconds) * time.Second
	}
	return time.Duration(n) * time.Second
}

func getInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func getBool(name string, fallback bool) bool {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}
