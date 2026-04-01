package httpapi

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

	resp, body := doJSON(t, ts, http.MethodGet, "/v1/mode", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["mode"].(string); got != "sse" {
		t.Fatalf("mode=%v want=sse body=%v", body["mode"], body)
	}
	if _, ok := body["poll_interval_ms"]; !ok {
		t.Fatalf("mode missing poll_interval_ms: %v", body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "agent-a"}, "")
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

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context a status=%d body=%v", resp.StatusCode, body)
	}
	bundleA := mustString(t, body, "bundle_hash")
	if _, ok := body["next_turn"]; !ok {
		t.Fatalf("context missing next_turn: %v", body)
	}
	if _, ok := body["next_actor_id"]; !ok {
		t.Fatalf("context missing next_actor_id: %v", body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "cipher-sql-1",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a turn0 status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 1,
		"ciphertext":    "cipher-sql-wrong-actor",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("message wrong actor status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["error"].(string); got != "turn_mismatch" {
		t.Fatalf("message wrong actor error=%v body=%v", body["error"], body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context b status=%d body=%v", resp.StatusCode, body)
	}
	bundleB := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 1,
		"ciphertext":    "cipher-sql-2",
		"bundle_hash":   bundleB,
	}, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message b turn1 status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 2,
		"ciphertext":    "cipher-sql-stale",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("message a stale hash status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["error"].(string); got != "stale_bundle_hash" {
		t.Fatalf("message stale hash error=%v body=%v", body["error"], body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/state", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("room state status=%d body=%v", resp.StatusCode, body)
	}
	if _, ok := body["next_turn"]; !ok {
		t.Fatalf("state missing next_turn: %v", body)
	}
	if _, ok := body["next_actor_id"]; !ok {
		t.Fatalf("state missing next_actor_id: %v", body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context a refresh status=%d body=%v", resp.StatusCode, body)
	}
	bundleA2 := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 2,
		"ciphertext":    "cipher-sql-3",
		"bundle_hash":   bundleA2,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a turn2 status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/close", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close room status=%d body=%v", resp.StatusCode, body)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/close", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second close room status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/transcript?human_code="+humanCode, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript status=%d body=%v", resp.StatusCode, body)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("unexpected transcript messages=%v", body["messages"])
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected first message payload=%v", msgs[0])
	}
	if _, ok := first["sender_name"].(string); !ok {
		t.Fatalf("sender_name missing in transcript message=%v", first)
	}

	rows, err := db.Query(`SELECT event_type FROM room_events WHERE room_id = $1 ORDER BY id ASC`, roomID)
	if err != nil {
		t.Fatalf("query room events: %v", err)
	}
	defer rows.Close()

	eventCounts := map[string]int{}
	for rows.Next() {
		var eventType string
		if scanErr := rows.Scan(&eventType); scanErr != nil {
			t.Fatalf("scan room event: %v", scanErr)
		}
		eventCounts[eventType]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate room events: %v", err)
	}
	if eventCounts["room.state_changed"] < 1 {
		t.Fatalf("missing room.state_changed event counts=%v", eventCounts)
	}
	if eventCounts["message.created"] != 3 {
		t.Fatalf("message.created count=%d want=3 events=%v", eventCounts["message.created"], eventCounts)
	}
	if eventCounts["room.closed"] != 1 {
		t.Fatalf("room.closed count=%d want=1 events=%v", eventCounts["room.closed"], eventCounts)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/admin/overview", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin overview status=%d body=%v", resp.StatusCode, body)
	}
	if _, ok := body["agents_total"]; !ok {
		t.Fatalf("admin overview missing agents_total: %v", body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/admin/rooms", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin rooms status=%d body=%v", resp.StatusCode, body)
	}
	if _, ok := body["items"].([]any); !ok {
		t.Fatalf("admin rooms missing items: %v", body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/admin/audit", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin audit status=%d body=%v", resp.StatusCode, body)
	}
	if _, ok := body["items"].([]any); !ok {
		t.Fatalf("admin audit missing items: %v", body)
	}
}

func TestSQLModeRoomEventsHistoryEndpoint(t *testing.T) {
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

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "events-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "events-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "events-history",
		"max_turns":   20,
		"ttl_seconds": 600,
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

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history", nil, tokenA)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("history before join status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/join", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join a status=%d body=%v", resp.StatusCode, body)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/join", nil, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join b status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context a status=%d body=%v", resp.StatusCode, body)
	}
	bundleA := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "events-cipher-1",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context b status=%d body=%v", resp.StatusCode, body)
	}
	bundleB := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 1,
		"ciphertext":    "events-cipher-2",
		"bundle_hash":   bundleB,
	}, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message b status=%d body=%v", resp.StatusCode, body)
	}

	if _, err := db.Exec(`INSERT INTO room_events (room_id, event_type) SELECT $1, 'test.synthetic' FROM generate_series(1, 210)`, roomID); err != nil {
		t.Fatalf("seed synthetic room events: %v", err)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history?limit=500", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history limit status=%d body=%v", resp.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("history items invalid payload=%v", body["items"])
	}
	if len(items) != 200 {
		t.Fatalf("history items len=%d want=200 (hard cap)", len(items))
	}

	nextSince, ok := body["next_since"].(float64)
	if !ok {
		t.Fatalf("next_since invalid payload=%v", body["next_since"])
	}
	prevID := int64(0)
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("history item %d invalid type=%T", i, raw)
		}
		idFloat, ok := item["event_id"].(float64)
		if !ok {
			t.Fatalf("history item %d missing event_id: %v", i, item)
		}
		id := int64(idFloat)
		if i > 0 && id <= prevID {
			t.Fatalf("history order not ascending at i=%d prev=%d curr=%d", i, prevID, id)
		}
		prevID = id
	}
	if prevID != int64(nextSince) {
		t.Fatalf("next_since=%d want=%d", int64(nextSince), prevID)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history?since="+strconv.FormatInt(int64(nextSince), 10), nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history resume status=%d body=%v", resp.StatusCode, body)
	}
	resumeItems, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("history resume items invalid payload=%v", body["items"])
	}
	if len(resumeItems) == 0 {
		t.Fatalf("history resume expected remaining items body=%v", body)
	}
	firstResume, ok := resumeItems[0].(map[string]any)
	if !ok {
		t.Fatalf("history resume first item invalid payload=%v", resumeItems[0])
	}
	firstResumeID, ok := firstResume["event_id"].(float64)
	if !ok || int64(firstResumeID) <= int64(nextSince) {
		t.Fatalf("history resume not exclusive since=%d first=%v", int64(nextSince), firstResume["event_id"])
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history", nil, "bad-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("history invalid token status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/room_missing/events/history", nil, tokenA)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("history missing room status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "events-history-2",
		"max_turns":   4,
		"ttl_seconds": 300,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing2 status=%d body=%v", resp.StatusCode, body)
	}
	listing2 := mustString(t, body, "id")
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listing2+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect listing2 status=%d body=%v", resp.StatusCode, body)
	}
	room2ID := mustString(t, body, "room_id")
	if _, err := db.Exec(`INSERT INTO room_events (room_id, event_type) VALUES ($1, 'test.room2')`, room2ID); err != nil {
		t.Fatalf("seed room2 event: %v", err)
	}
	var room2EventID int64
	if err := db.QueryRow(`SELECT id FROM room_events WHERE room_id = $1 ORDER BY id ASC LIMIT 1`, room2ID).Scan(&room2EventID); err != nil {
		t.Fatalf("query room2 event id: %v", err)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history?since="+strconv.FormatInt(room2EventID, 10), nil, tokenA)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("history since from different room status=%d body=%v", resp.StatusCode, body)
	}

	if _, err := db.Exec(`UPDATE rooms SET state = 'PURGED', purged_at = NOW() WHERE id = $1`, roomID); err != nil {
		t.Fatalf("mark room purged: %v", err)
	}
	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history", nil, tokenA)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("history purged room status=%d body=%v", resp.StatusCode, body)
	}
}

func TestSQLModeRoomEventsSSEEndpoint(t *testing.T) {
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

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "sse-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "sse-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "sse-events",
		"max_turns":   8,
		"ttl_seconds": 600,
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

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/join", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join a status=%d body=%v", resp.StatusCode, body)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/join", nil, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join b status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history?limit=1", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history baseline status=%d body=%v", resp.StatusCode, body)
	}
	baseline := int64(0)
	if next, ok := body["next_since"].(float64); ok {
		baseline = int64(next)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/rooms/"+roomID+"/events?since="+strconv.FormatInt(baseline, 10), nil)
	if err != nil {
		t.Fatalf("new sse request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenA)
	sseResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("open sse stream: %v", err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(sseResp.Body)
		t.Fatalf("sse status=%d body=%s", sseResp.StatusCode, string(body))
	}
	if got := sseResp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("sse content-type=%q", got)
	}

	reader := bufio.NewReader(sseResp.Body)
	eventCh, errCh := startSSEEventStream(reader)

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 7,
		"ciphertext":    "sse-invalid-turn",
		"bundle_hash":   "invalid-bundle-hash",
	}, tokenA)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("invalid message status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["error"].(string); got != "turn_mismatch" {
		t.Fatalf("invalid message error=%v body=%v", body["error"], body)
	}
	expectNoSSEEvent(t, eventCh, errCh, 1200*time.Millisecond)

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context a status=%d body=%v", resp.StatusCode, body)
	}
	bundleA := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "sse-cipher-1",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a status=%d body=%v", resp.StatusCode, body)
	}

	msgEvent := waitForSSEEventType(t, eventCh, errCh, "message.created", 8*time.Second)
	if msgEvent.RoomID != roomID {
		t.Fatalf("sse message event room_id=%q want=%q", msgEvent.RoomID, roomID)
	}
	if msgEvent.SenderID == nil || *msgEvent.SenderID == "" {
		t.Fatalf("sse message event sender missing: %+v", msgEvent)
	}
	if msgEvent.Ciphertext == nil || *msgEvent.Ciphertext != "sse-cipher-1" {
		t.Fatalf("sse message event ciphertext mismatch: %+v", msgEvent)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/close", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close room status=%d body=%v", resp.StatusCode, body)
	}
	closedEvent := waitForSSEEventType(t, eventCh, errCh, "room.closed", 8*time.Second)
	if closedEvent.RoomID != roomID {
		t.Fatalf("sse closed event room_id=%q want=%q", closedEvent.RoomID, roomID)
	}
}

type sseEventEnvelope struct {
	EventID    int64   `json:"event_id"`
	Type       string  `json:"type"`
	RoomID     string  `json:"room_id"`
	MessageID  string  `json:"message_id"`
	Turn       *int    `json:"turn"`
	SenderID   *string `json:"sender_id"`
	Ciphertext *string `json:"ciphertext"`
}

func startSSEEventStream(reader *bufio.Reader) (<-chan sseEventEnvelope, <-chan error) {
	events := make(chan sseEventEnvelope, 32)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		for {
			ev, err := readSSEFrame(reader)
			if err != nil {
				if err == io.EOF {
					return
				}
				errs <- err
				return
			}
			events <- ev
		}
	}()
	return events, errs
}

func waitForSSEEventType(t *testing.T, eventCh <-chan sseEventEnvelope, errCh <-chan error, eventType string, timeout time.Duration) sseEventEnvelope {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case err := <-errCh:
			t.Fatalf("sse stream error: %v", err)
		case ev, ok := <-eventCh:
			if !ok {
				t.Fatalf("sse stream closed while waiting for event type %q", eventType)
			}
			if ev.Type == eventType {
				return ev
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for SSE event type %q", eventType)
		}
	}
}

func expectNoSSEEvent(t *testing.T, eventCh <-chan sseEventEnvelope, errCh <-chan error, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-errCh:
		t.Fatalf("sse stream error while expecting silence: %v", err)
	case ev, ok := <-eventCh:
		if !ok {
			return
		}
		t.Fatalf("unexpected sse event type=%q id=%d", ev.Type, ev.EventID)
	case <-timer.C:
	}
}

func readSSEFrame(reader *bufio.Reader) (sseEventEnvelope, error) {
	for {
		var (
			eventID   int64
			eventType string
			dataBuf   strings.Builder
		)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return sseEventEnvelope{}, err
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if eventType == "" || dataBuf.Len() == 0 {
					break
				}
				var payload sseEventEnvelope
				if err := json.Unmarshal([]byte(dataBuf.String()), &payload); err != nil {
					return sseEventEnvelope{}, fmt.Errorf("unmarshal data: %w", err)
				}
				if eventID > 0 {
					payload.EventID = eventID
				}
				if eventType != "" {
					payload.Type = eventType
				}
				return payload, nil
			}
			if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "retry:") {
				continue
			}
			if strings.HasPrefix(line, "id:") {
				raw := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
				v, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					return sseEventEnvelope{}, fmt.Errorf("parse id: %w", err)
				}
				eventID = v
				continue
			}
			if strings.HasPrefix(line, "event:") {
				eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if strings.HasPrefix(line, "data:") {
				if dataBuf.Len() > 0 {
					dataBuf.WriteByte('\n')
				}
				dataBuf.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
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

	up2, err := os.ReadFile(filepath.Join(migDir, "000002_room_context_state.up.sql"))
	if err != nil {
		t.Fatalf("read room context up migration: %v", err)
	}
	up3, err := os.ReadFile(filepath.Join(migDir, "000003_room_events.up.sql"))
	if err != nil {
		t.Fatalf("read room events up migration: %v", err)
	}

	if _, err := db.Exec(string(down)); err != nil {
		t.Fatalf("exec down migration: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_events`); err != nil {
		t.Fatalf("cleanup room events: %v", err)
	}
	if _, err := db.Exec(string(up)); err != nil {
		t.Fatalf("exec up migration: %v", err)
	}
	if _, err := db.Exec(string(up2)); err != nil {
		t.Fatalf("exec room context up migration: %v", err)
	}
	if _, err := db.Exec(string(up3)); err != nil {
		t.Fatalf("exec room events up migration: %v", err)
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
