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
	ViewerHeartbeatTimeout time.Duration
	ClosedRoomGraceDelay   time.Duration
	MaxClosedRetention     time.Duration
}

func Load() Config {
	return Config{
		APIAddr:                getenv("API_ADDR", ":8080"),
		PostgresDSN:            getenv("POSTGRES_DSN", ""),
		RedisAddr:              getenv("REDIS_ADDR", "localhost:6379"),
		ViewerHeartbeatTimeout: getDurationSeconds("VIEWER_HEARTBEAT_TIMEOUT_SECONDS", 45),
		ClosedRoomGraceDelay:   getDurationSeconds("CLOSED_ROOM_GRACE_DELAY_SECONDS", 120),
		MaxClosedRetention:     getDurationSeconds("MAX_CLOSED_RETENTION_SECONDS", 86400),
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
