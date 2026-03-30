package httpapi

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/repository/postgres"
	_ "github.com/lib/pq"
)

func TestSQLModeListingConnectAndTranscriptFlow(t *testing.T) {
	t.Parallel()

	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN (or POSTGRES_DSN) to run SQL integration test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}

	applyMigrationsForTest(t, db)

	store := postgres.NewStore(db)
	ts := httptest.NewServer(NewRouterWithStore(store, 45*time.Second, 2*time.Minute, 24*time.Hour))
	defer ts.Close()

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "agent-a"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register a status=%d body=%v", resp.StatusCode, body)
	}
	apiA := mustString(t, body, "api_key")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "agent-b"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register b status=%d body=%v", resp.StatusCode, body)
	}
	apiB := mustString(t, body, "api_key")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login a status=%d body=%v", resp.StatusCode, body)
	}
	tokenA := mustString(t, body, "session_token")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login b status=%d body=%v", resp.StatusCode, body)
	}
	tokenB := mustString(t, body, "session_token")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "sql mode room",
		"tags":        []string{"sql", "integration"},
		"max_turns":   4,
		"ttl_seconds": 300,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")
	humanCode := mustString(t, body, "human_code")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/join", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join a status=%d body=%v", resp.StatusCode, body)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/join", nil, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join b status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "cipher-sql-1",
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a turn0 status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 1,
		"ciphertext":    "cipher-sql-2",
	}, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message b turn1 status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/transcript?human_code="+humanCode, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript status=%d body=%v", resp.StatusCode, body)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("unexpected transcript messages=%v", body["messages"])
	}
}

func applyMigrationsForTest(t *testing.T, db *sql.DB) {
	t.Helper()

	migDir := migrationsDir(t)
	down, err := os.ReadFile(filepath.Join(migDir, "000001_init.down.sql"))
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	up, err := os.ReadFile(filepath.Join(migDir, "000001_init.up.sql"))
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}

	if _, err := db.Exec(string(down)); err != nil {
		t.Fatalf("exec down migration: %v", err)
	}
	if _, err := db.Exec(string(up)); err != nil {
		t.Fatalf("exec up migration: %v", err)
	}
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve runtime caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "migrations"))
}
