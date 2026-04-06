package httpapi

import (
	"bufio"
	"context"
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

	"github.com/febrian/areyouai/internal/repository"
	"github.com/febrian/areyouai/internal/repository/postgres"
	"github.com/febrian/areyouai/internal/security"
	_ "github.com/lib/pq"
)

type failingRoomContextStore struct {
	repository.Store
}

func (s failingRoomContextStore) UpsertRoomContext(ctx context.Context, in repository.UpsertRoomContextInput) (repository.RoomContextState, error) {
	return repository.RoomContextState{}, fmt.Errorf("forced room context failure for test")
}

type alwaysFailRoomContextStore struct {
	repository.Store
}

func (s alwaysFailRoomContextStore) UpsertRoomContext(ctx context.Context, in repository.UpsertRoomContextInput) (repository.RoomContextState, error) {
	return repository.RoomContextState{}, fmt.Errorf("forced room context ack failure for test")
}

type failingRoomContextReadStore struct {
	repository.Store
}

func (s failingRoomContextReadStore) GetRoomContext(ctx context.Context, roomID string) (repository.RoomContextState, error) {
	return repository.RoomContextState{}, fmt.Errorf("forced room context read failure for test")
}

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
	ts := httptest.NewServer(NewRouterWithStore(store, 3*time.Second, 2*time.Minute, 24*time.Hour))
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
	roomID := mustString(t, body, "room_id")
	humanCode := mustString(t, body, "human_code")
	if got, _ := body["owner_joined"].(bool); !got {
		t.Fatalf("owner_joined=%v body=%v", body["owner_joined"], body)
	}
	if got, _ := body["room_state"].(string); got != "OPEN" {
		t.Fatalf("create listing room_state=%v body=%v", body["room_state"], body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "before-connect",
		"bundle_hash":   "preconnect",
	}, tokenA)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("message before connect status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["room_state"].(string); got != "ACTIVE" {
		t.Fatalf("connect room_state=%v body=%v", body["room_state"], body)
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
	contextA := body
	bundleA := mustString(t, contextA, "bundle_hash")
	turnIndexA, ok := contextA["turn_index"].(float64)
	if !ok {
		t.Fatalf("context missing turn_index: %v", contextA)
	}
	if _, ok := contextA["next_turn"]; !ok {
		t.Fatalf("context missing next_turn: %v", contextA)
	}
	if _, ok := contextA["next_actor_id"]; !ok {
		t.Fatalf("context missing next_actor_id: %v", contextA)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/context/ack", map[string]any{"turn_index": turnIndexA}, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context ack a status=%d body=%v", resp.StatusCode, body)
	}
	promptBundle := mustString(t, contextA, "prompt_bundle_text")
	if !strings.Contains(promptBundle, "room_topic=sql mode room") {
		t.Fatalf("context missing room topic anchor: %s", promptBundle)
	}
	if !strings.Contains(promptBundle, "conversation_mode=normal_chat") {
		t.Fatalf("context missing conversation mode anchor: %s", promptBundle)
	}
	if !strings.Contains(promptBundle, "conversation_summary=topic=sql mode room | mode=normal_chat | recent=none") {
		t.Fatalf("context missing conversation summary anchor: %s", promptBundle)
	}
	if !strings.Contains(promptBundle, "interaction_anchor=Advance the discussion naturally; avoid empty agreement, empty praise, or paraphrase-only turns.") {
		t.Fatalf("context missing interaction anchor: %s", promptBundle)
	}
	if !strings.Contains(promptBundle, "voice_hint=") {
		t.Fatalf("context missing voice hint: %s", promptBundle)
	}

	overSizedCipher := strings.Repeat("x", security.MaxPersistMessageChars+1)
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    overSizedCipher,
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized message status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["error"].(string); got != "payload_too_large" {
		t.Fatalf("oversized message error=%v body=%v", body["error"], body)
	}
	if got, _ := body["max_chars"].(float64); got != security.MaxPersistMessageChars {
		t.Fatalf("oversized message max_chars=%v body=%v", got, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "cipher-sql-1",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a turn0 status=%d body=%v", resp.StatusCode, body)
	}
	var storedMessageCipher string
	if err := db.QueryRow(`SELECT ciphertext FROM messages WHERE room_id = $1 AND turn = 0`, roomID).Scan(&storedMessageCipher); err != nil {
		t.Fatalf("query stored message ciphertext: %v", err)
	}
	if storedMessageCipher == "cipher-sql-1" {
		t.Fatalf("stored message remained plaintext: %q", storedMessageCipher)
	}
	if !strings.HasPrefix(storedMessageCipher, "enc:v1:") {
		t.Fatalf("stored message ciphertext prefix mismatch: %q", storedMessageCipher)
	}
	var storedRoomKey string
	if err := db.QueryRow(`SELECT message_key_ciphertext FROM rooms WHERE id = $1`, roomID).Scan(&storedRoomKey); err != nil {
		t.Fatalf("query stored room key: %v", err)
	}
	if storedRoomKey == "" {
		t.Fatal("expected wrapped room message key to be stored")
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
	turnIndexB, ok := body["turn_index"].(float64)
	if !ok {
		t.Fatalf("context missing turn_index: %v", body)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/context/ack", map[string]any{"turn_index": turnIndexB}, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context ack b status=%d body=%v", resp.StatusCode, body)
	}

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
	turnIndexA2, ok := body["turn_index"].(float64)
	if !ok {
		t.Fatalf("context missing turn_index: %v", body)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/context/ack", map[string]any{"turn_index": turnIndexA2}, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context ack a2 status=%d body=%v", resp.StatusCode, body)
	}

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

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/transcript", map[string]any{"human_code": humanCode}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript status=%d body=%v", resp.StatusCode, body)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("unexpected transcript messages=%v", body["messages"])
	}
	if got, _ := body["room_topic"].(string); got != "sql mode room" {
		t.Fatalf("room_topic=%q want=%q body=%v", got, "sql mode room", body)
	}
	if got, _ := body["agent_a_id"].(string); got == "" {
		t.Fatalf("agent_a_id missing in transcript body=%v", body)
	}
	if got, _ := body["agent_b_id"].(string); got == "" {
		t.Fatalf("agent_b_id missing in transcript body=%v", body)
	}
	if got, _ := body["turn_index"].(float64); got != 3 {
		t.Fatalf("turn_index=%v want=3 body=%v", got, body)
	}
	if _, ok := body["next_actor_id"]; !ok {
		t.Fatalf("next_actor_id missing in transcript body=%v", body)
	}
	if fetchMap, ok := body["last_context_fetch_turn_by_agent"].(map[string]any); !ok || len(fetchMap) == 0 {
		t.Fatalf("missing context fetch map in transcript body=%v", body)
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected first message payload=%v", msgs[0])
	}
	if _, ok := first["sender_name"].(string); !ok {
		t.Fatalf("sender_name missing in transcript message=%v", first)
	}
	if got, _ := first["read_by_opponent"].(bool); !got {
		t.Fatalf("first message should be read by opponent: %v", first)
	}
	second, ok := msgs[1].(map[string]any)
	if !ok {
		t.Fatalf("unexpected second message payload=%v", msgs[1])
	}
	if got, _ := second["read_by_opponent"].(bool); !got {
		t.Fatalf("second message should be read by opponent: %v", second)
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
	if _, err := db.Exec(`UPDATE rooms SET closed_at = NOW() - INTERVAL '5 minutes' WHERE id = $1`, roomID); err != nil {
		t.Fatalf("backdate closed room: %v", err)
	}
	resp, body = doJSON(t, ts, http.MethodGet, "/v1/admin/overview", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin overview status=%d body=%v", resp.StatusCode, body)
	}
	purge, ok := body["purge"].(map[string]any)
	if !ok {
		t.Fatalf("admin overview missing purge telemetry: %v", body)
	}
	if ready, _ := purge["ready_for_purge"].(float64); ready < 1 {
		t.Fatalf("purge ready_for_purge=%v want>=1 purge=%v", purge["ready_for_purge"], purge)
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
	ts := httptest.NewServer(NewRouterWithStore(store, 3*time.Second, 2*time.Minute, 24*time.Hour))
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history after auto-join status=%d body=%v", resp.StatusCode, body)
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

func TestSQLModeCreateAndConnectSucceedWhenRoomContextSyncFails(t *testing.T) {
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

	store := failingRoomContextStore{Store: postgres.NewStore(db)}
	ts := httptest.NewServer(NewRouterWithStore(store, 45*time.Second, 2*time.Minute, 24*time.Hour))
	defer ts.Close()

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "failing-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "failing-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "forced-context-failure",
		"max_turns":   6,
		"ttl_seconds": 300,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")
	roomID := mustString(t, body, "room_id")
	if humanCode := mustString(t, body, "human_code"); humanCode == "" {
		t.Fatalf("create listing returned empty human_code")
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}
	if got := mustString(t, body, "room_id"); got != roomID {
		t.Fatalf("connect room_id=%s want=%s", got, roomID)
	}

	var (
		storedRoomID string
		connected    bool
	)
	if err := db.QueryRow(`SELECT COALESCE(room_id, ''), connected FROM chat_listings WHERE id = $1`, listingID).Scan(&storedRoomID, &connected); err != nil {
		t.Fatalf("query listing: %v", err)
	}
	if storedRoomID != roomID {
		t.Fatalf("stored room_id=%s want=%s", storedRoomID, roomID)
	}
	if !connected {
		t.Fatalf("listing %s should be connected after successful /connect", listingID)
	}
}

func TestSQLModeContextReturnsPromptBundleWhenContextAckFails(t *testing.T) {
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

	store := alwaysFailRoomContextStore{Store: postgres.NewStore(db)}
	ts := httptest.NewServer(NewRouterWithStore(store, 45*time.Second, 2*time.Minute, 24*time.Hour))
	defer ts.Close()

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "context-failing-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "context-failing-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "context best effort",
		"max_turns":   4,
		"ttl_seconds": 300,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")
	humanCode := mustString(t, body, "human_code")
	listingID := mustString(t, body, "id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context status=%d body=%v", resp.StatusCode, body)
	}
	if got := mustString(t, body, "room_id"); got != roomID {
		t.Fatalf("context room_id=%s want=%s", got, roomID)
	}
	if mustString(t, body, "bundle_hash") == "" {
		t.Fatal("context returned empty bundle_hash")
	}
	if got := mustString(t, body, "next_actor_id"); got == "" {
		t.Fatalf("context returned empty next_actor_id body=%v", body)
	}
	turnIndex, ok := body["turn_index"].(float64)
	if !ok {
		t.Fatalf("context missing turn_index: %v", body)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/context/ack", map[string]any{"turn_index": turnIndex}, tokenA)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("context ack status=%d body=%v", resp.StatusCode, body)
	}
	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/transcript", map[string]any{"human_code": humanCode}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript after failed ack status=%d body=%v", resp.StatusCode, body)
	}
	fetchMap, ok := body["last_context_fetch_turn_by_agent"].(map[string]any)
	if !ok && body["last_context_fetch_turn_by_agent"] != nil {
		t.Fatalf("unexpected fetch map payload=%v", body["last_context_fetch_turn_by_agent"])
	}
	if ok && len(fetchMap) != 0 {
		t.Fatalf("expected empty fetch map after failed ack: %v", fetchMap)
	}
}

func TestSQLModeTranscriptSurvivesRoomContextReadFailure(t *testing.T) {
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

	store := failingRoomContextReadStore{Store: postgres.NewStore(db)}
	ts := httptest.NewServer(NewRouterWithStore(store, 45*time.Second, 2*time.Minute, 24*time.Hour))
	defer ts.Close()

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "transcript-failing-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "transcript-failing-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "transcript best effort",
		"max_turns":   2,
		"ttl_seconds": 300,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")
	humanCode := mustString(t, body, "human_code")
	listingID := mustString(t, body, "id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/transcript", map[string]any{"human_code": humanCode}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript status=%d body=%v", resp.StatusCode, body)
	}
	if got := mustString(t, body, "room_id"); got != roomID {
		t.Fatalf("transcript room_id=%s want=%s", got, roomID)
	}
	fetchMap, ok := body["last_context_fetch_turn_by_agent"].(map[string]any)
	if !ok && body["last_context_fetch_turn_by_agent"] != nil {
		t.Fatalf("unexpected fetch map payload=%v", body["last_context_fetch_turn_by_agent"])
	}
	if ok && len(fetchMap) != 0 {
		t.Fatalf("expected empty fetch map when room context read fails: %v", fetchMap)
	}
}

func TestSQLModeRoomEventsHistoryAutoJoinSurvivesNewInstance(t *testing.T) {
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
	tsA := httptest.NewServer(NewRouterWithStore(store, 45*time.Second, 2*time.Minute, 24*time.Hour))
	defer tsA.Close()
	tsB := httptest.NewServer(NewRouterWithStore(store, 45*time.Second, 2*time.Minute, 24*time.Hour))
	defer tsB.Close()

	_, body := doJSON(t, tsA, http.MethodPost, "/v1/agent/register", map[string]any{"name": "restart-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, tsA, http.MethodPost, "/v1/agent/register", map[string]any{"name": "restart-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, tsA, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, tsA, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, tsA, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "cross-instance-history",
		"max_turns":   8,
		"ttl_seconds": 600,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")
	roomID := mustString(t, body, "room_id")

	resp, body = doJSON(t, tsA, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, tsB, http.MethodGet, "/v1/rooms/"+roomID+"/events/history", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history owner on new instance status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, tsB, http.MethodGet, "/v1/rooms/"+roomID+"/events/history", nil, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history connected peer on new instance status=%d body=%v", resp.StatusCode, body)
	}
}

func TestSQLModeWebhookOutboxEmission(t *testing.T) {
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

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "outbox-a"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register a status=%d body=%v", resp.StatusCode, body)
	}
	apiA := mustString(t, body, "api_key")
	agentAID := mustString(t, body, "agent_id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "outbox-b"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register b status=%d body=%v", resp.StatusCode, body)
	}
	apiB := mustString(t, body, "api_key")
	agentBID := mustString(t, body, "agent_id")

	if _, err := store.CreateAgentWebhookEndpoint(context.Background(), repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_outbox_a",
		AgentID:          agentAID,
		URL:              "https://example.com/hooks/a",
		SecretCiphertext: "enc-a",
		KeyID:            "key-a",
		Enabled:          true,
	}); err != nil {
		t.Fatalf("create webhook endpoint a: %v", err)
	}
	if _, err := store.CreateAgentWebhookEndpoint(context.Background(), repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_outbox_b",
		AgentID:          agentBID,
		URL:              "https://example.com/hooks/b",
		SecretCiphertext: "enc-b",
		KeyID:            "key-b",
		Enabled:          true,
	}); err != nil {
		t.Fatalf("create webhook endpoint b: %v", err)
	}

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
		"topic":       "webhook-outbox",
		"max_turns":   6,
		"ttl_seconds": 300,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")
	roomID := mustString(t, body, "room_id")

	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM webhook_outbox`).Scan(&count); err != nil {
		t.Fatalf("count webhook outbox after create: %v", err)
	}
	if count != 0 {
		t.Fatalf("webhook outbox count after create=%d want=0", count)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context a status=%d body=%v", resp.StatusCode, body)
	}
	bundleA := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "outbox-message",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/close", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close room status=%d body=%v", resp.StatusCode, body)
	}

	rows, err := db.Query(`
SELECT target_agent_id, event_type, payload
FROM webhook_outbox
WHERE room_id = $1
ORDER BY id ASC`, roomID)
	if err != nil {
		t.Fatalf("query webhook outbox: %v", err)
	}
	defer rows.Close()

	type outboxRow struct {
		targetAgentID string
		eventType     string
		payload       map[string]any
	}
	var items []outboxRow
	for rows.Next() {
		var targetAgentID, eventType string
		var raw []byte
		if err := rows.Scan(&targetAgentID, &eventType, &raw); err != nil {
			t.Fatalf("scan webhook outbox row: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode webhook payload: %v", err)
		}
		items = append(items, outboxRow{
			targetAgentID: targetAgentID,
			eventType:     eventType,
			payload:       payload,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate webhook outbox rows: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("webhook outbox len=%d want=3 items=%v", len(items), items)
	}

	if items[0].eventType != "room.joined" || items[0].targetAgentID != agentAID {
		t.Fatalf("first outbox row=%+v want room.joined target=%s", items[0], agentAID)
	}
	if got := mustPayloadString(t, items[0].payload, "next_actor_id"); got != agentAID {
		t.Fatalf("room.joined next_actor_id=%s want=%s payload=%v", got, agentAID, items[0].payload)
	}

	if items[1].eventType != "message.created" || items[1].targetAgentID != agentBID {
		t.Fatalf("second outbox row=%+v want message.created target=%s", items[1], agentBID)
	}
	if got := mustPayloadString(t, items[1].payload, "next_actor_id"); got != agentBID {
		t.Fatalf("message.created next_actor_id=%s want=%s payload=%v", got, agentBID, items[1].payload)
	}

	if items[2].eventType != "room.closed" || items[2].targetAgentID != agentBID {
		t.Fatalf("third outbox row=%+v want room.closed target=%s", items[2], agentBID)
	}
	if got, ok := items[2].payload["next_actor_id"]; ok && got != "" {
		t.Fatalf("room.closed next_actor_id=%v want empty/absent payload=%v", got, items[2].payload)
	}
}

func TestSQLModeAgentWebhookEndpointsAPI(t *testing.T) {
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

	resp, body := doJSON(t, ts, http.MethodGet, "/v1/agent/webhooks", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list webhook endpoints unauth status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "webhook-api-agent"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status=%d body=%v", resp.StatusCode, body)
	}
	apiKey := mustString(t, body, "api_key")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiKey}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d body=%v", resp.StatusCode, body)
	}
	token := mustString(t, body, "session_token")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/webhooks", map[string]any{
		"url":    "http://example.com/hooks/agent",
		"secret": "super-secret",
	}, token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create invalid webhook status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["error"].(string); got != "invalid_request" {
		t.Fatalf("create invalid webhook error=%v body=%v", body["error"], body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/webhooks", map[string]any{
		"url":     "https://example.com/hooks/agent",
		"secret":  "super-secret",
		"key_id":  "kid-api",
		"enabled": true,
	}, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create webhook status=%d body=%v", resp.StatusCode, body)
	}
	endpointID := mustString(t, body, "id")
	if got := mustString(t, body, "url"); got != "https://example.com/hooks/agent" {
		t.Fatalf("created url=%s want=https://example.com/hooks/agent", got)
	}
	if _, ok := body["secret"]; ok {
		t.Fatalf("webhook create response leaked secret: %v", body)
	}
	var storedSecret string
	if err := db.QueryRow(`SELECT secret_ciphertext FROM agent_webhook_endpoints WHERE id = $1`, endpointID).Scan(&storedSecret); err != nil {
		t.Fatalf("query stored webhook secret: %v", err)
	}
	if storedSecret == "super-secret" {
		t.Fatalf("stored webhook secret remained plaintext: %q", storedSecret)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/agent/webhooks", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list webhook endpoints status=%d body=%v", resp.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("list webhook endpoints items=%v", body["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("list webhook endpoints first item=%v", items[0])
	}
	if got := mustString(t, item, "id"); got != endpointID {
		t.Fatalf("listed endpoint id=%s want=%s", got, endpointID)
	}

	resp, body = doJSON(t, ts, http.MethodDelete, "/v1/agent/webhooks/"+endpointID, nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete webhook endpoint status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/agent/webhooks", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list webhook endpoints after delete status=%d body=%v", resp.StatusCode, body)
	}
	items, ok = body["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("list webhook endpoints after delete items=%v", body["items"])
	}

	resp, body = doJSON(t, ts, http.MethodDelete, "/v1/agent/webhooks/"+endpointID, nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing webhook endpoint status=%d body=%v", resp.StatusCode, body)
	}
}

func TestSQLModeLegacyListingConnectEmitsLifecycleEvents(t *testing.T) {
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

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "legacy-owner"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register owner status=%d body=%v", resp.StatusCode, body)
	}
	ownerAPIKey := mustString(t, body, "api_key")
	ownerAgentID := mustString(t, body, "agent_id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "legacy-joiner"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register joiner status=%d body=%v", resp.StatusCode, body)
	}
	joinerAPIKey := mustString(t, body, "api_key")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": ownerAPIKey}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login owner status=%d body=%v", resp.StatusCode, body)
	}
	ownerToken := mustString(t, body, "session_token")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": joinerAPIKey}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login joiner status=%d body=%v", resp.StatusCode, body)
	}
	joinerToken := mustString(t, body, "session_token")

	if _, err := store.CreateAgentWebhookEndpoint(context.Background(), repository.CreateAgentWebhookEndpointInput{
		ID:               "wh_ep_legacy_owner",
		AgentID:          ownerAgentID,
		URL:              "https://example.com/hooks/legacy-owner",
		SecretCiphertext: "enc-legacy-owner",
		KeyID:            "key-legacy-owner",
		Enabled:          true,
	}); err != nil {
		t.Fatalf("create owner webhook endpoint: %v", err)
	}

	legacyListing, err := store.CreateListing(context.Background(), repository.CreateListingInput{
		ID:         "lst_legacy_connect",
		AgentID:    ownerAgentID,
		Topic:      "legacy listing without room",
		Tags:       []string{"legacy"},
		MaxTurns:   8,
		TTLSeconds: 600,
		RoomID:     "",
	})
	if err != nil {
		t.Fatalf("create legacy listing: %v", err)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+legacyListing.ID+"/connect", nil, joinerToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect legacy listing status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history?since=0&limit=20", nil, ownerToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy room history status=%d body=%v", resp.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("legacy room history items=%v", body["items"])
	}
	if len(items) != 2 {
		t.Fatalf("legacy room history len=%d want=2 items=%v", len(items), items)
	}
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if got := mustString(t, first, "type"); got != "room.joined" {
		t.Fatalf("legacy first event type=%s want=room.joined", got)
	}
	if got := mustString(t, second, "type"); got != "room.state_changed" {
		t.Fatalf("legacy second event type=%s want=room.state_changed", got)
	}

	var outboxCount int
	if err := db.QueryRow(`SELECT COUNT(1) FROM webhook_outbox WHERE room_id = $1 AND target_agent_id = $2 AND event_type = 'room.joined'`, roomID, ownerAgentID).Scan(&outboxCount); err != nil {
		t.Fatalf("count owner legacy outbox rows: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("owner legacy outbox count=%d want=1", outboxCount)
	}
}

func TestSQLModeListingSearchHandlesNilAndLegacyNullTags(t *testing.T) {
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
	ts := httptest.NewServer(NewRouterWithStore(store, 3*time.Second, 2*time.Minute, 24*time.Hour))
	defer ts.Close()

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "search-owner"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register owner status=%d body=%v", resp.StatusCode, body)
	}
	ownerAPIKey := mustString(t, body, "api_key")
	ownerAgentID := mustString(t, body, "agent_id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": ownerAPIKey}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login owner status=%d body=%v", resp.StatusCode, body)
	}
	ownerToken := mustString(t, body, "session_token")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "fresh listing without tags",
		"max_turns":   4,
		"ttl_seconds": 300,
	}, ownerToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")
	tags, ok := body["tags"].([]any)
	if !ok {
		t.Fatalf("create listing tags=%T want []any body=%v", body["tags"], body)
	}
	if len(tags) != 0 {
		t.Fatalf("create listing tags len=%d want=0 body=%v", len(tags), body)
	}

	var storedTags string
	if err := db.QueryRow(`SELECT tags::text FROM chat_listings WHERE id = $1`, listingID).Scan(&storedTags); err != nil {
		t.Fatalf("query stored tags: %v", err)
	}
	if storedTags != "[]" {
		t.Fatalf("stored tags=%q want=[]", storedTags)
	}

	if _, err := db.Exec(
		`INSERT INTO chat_listings (id, agent_id, topic, tags, max_turns, ttl_seconds, connected, room_id)
		 VALUES ($1, $2, $3, 'null'::jsonb, $4, $5, FALSE, NULL)`,
		"lst_legacy_null_tags",
		ownerAgentID,
		"legacy null tags listing",
		4,
		300,
	); err != nil {
		t.Fatalf("insert legacy null tags listing: %v", err)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/listings/search?q=fresh", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search fresh status=%d body=%v", resp.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("search items=%T want []any body=%v", body["items"], body)
	}
	if len(items) != 1 {
		t.Fatalf("search items len=%d want=1 body=%v", len(items), body)
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("search item type=%T body=%v", items[0], body)
	}
	if got := mustString(t, first, "id"); got != listingID {
		t.Fatalf("search item id=%s want=%s body=%v", got, listingID, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/listings/search?q=", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search empty status=%d body=%v", resp.StatusCode, body)
	}
	items, ok = body["items"].([]any)
	if !ok {
		t.Fatalf("search empty items=%T want []any body=%v", body["items"], body)
	}
	if len(items) != 2 {
		t.Fatalf("search empty items len=%d want=2 body=%v", len(items), body)
	}
}

func TestSQLModeTypingIndicatorViewerStream(t *testing.T) {
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

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "typing-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "typing-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "typing indicator",
		"max_turns":   1,
		"ttl_seconds": 600,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")
	humanCode := mustString(t, body, "human_code")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/viewers", map[string]any{
		"op":         "join",
		"human_code": humanCode,
	}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("viewer join status=%d body=%v", resp.StatusCode, body)
	}
	viewerToken := mustString(t, body, "viewer_token")

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context status=%d body=%v", resp.StatusCode, body)
	}
	bundleHash := mustString(t, body, "bundle_hash")

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/rooms/"+roomID+"/viewer-events", nil)
	if err != nil {
		t.Fatalf("new viewer stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+viewerToken)
	sseResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("open viewer stream: %v", err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(sseResp.Body)
		t.Fatalf("viewer stream status=%d body=%s", sseResp.StatusCode, string(body))
	}
	if got := sseResp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("viewer stream content-type=%q", got)
	}

	reader := bufio.NewReader(sseResp.Body)
	eventCh, errCh := startGenericSSEStream(reader)

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/typing", map[string]any{
		"state":  "start",
		"ttl_ms": 2000,
	}, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("typing start status=%d body=%v", resp.StatusCode, body)
	}
	startEvent := waitForGenericSSEEventType(t, eventCh, errCh, "agent.typing", 5*time.Second)
	if got, _ := startEvent.Payload["state"].(string); got != "start" {
		t.Fatalf("typing start state=%v payload=%v", startEvent.Payload["state"], startEvent.Payload)
	}
	if got, _ := startEvent.Payload["actor_id"].(string); got != mustString(t, body, "actor_id") {
		t.Fatalf("typing start actor_id=%v payload=%v", startEvent.Payload["actor_id"], startEvent.Payload)
	}
	if _, ok := startEvent.Payload["expires_at"]; !ok {
		t.Fatalf("typing start missing expires_at payload=%v", startEvent.Payload)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/typing", map[string]any{
		"state": "start",
	}, tokenB)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("typing wrong actor status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["error"].(string); got != "turn_mismatch" {
		t.Fatalf("typing wrong actor error=%v body=%v", body["error"], body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "typing clears on message",
		"bundle_hash":   bundleHash,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message status=%d body=%v", resp.StatusCode, body)
	}
	stopEvent := waitForGenericSSEEventType(t, eventCh, errCh, "agent.typing", 5*time.Second)
	if got, _ := stopEvent.Payload["state"].(string); got != "stop" {
		t.Fatalf("typing stop state=%v payload=%v", stopEvent.Payload["state"], stopEvent.Payload)
	}
	if got, _ := stopEvent.Payload["actor_id"].(string); got != startEvent.Payload["actor_id"] {
		t.Fatalf("typing stop actor_id=%v start=%v", stopEvent.Payload["actor_id"], startEvent.Payload["actor_id"])
	}

	staleAt := time.Now().UTC().Add(-30 * time.Second)
	if _, err := db.ExecContext(context.Background(), `UPDATE room_viewers SET last_heartbeat_at = $1 WHERE viewer_token = $2`, staleAt, viewerToken); err != nil {
		t.Fatalf("stale viewer heartbeat: %v", err)
	}
	waitForGenericSSEStreamClose(t, eventCh, errCh, 3*time.Second)
}

func TestSQLModeViewerEventsRejectExpiredViewerOnOpen(t *testing.T) {
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
	ts := httptest.NewServer(NewRouterWithStore(store, 3*time.Second, 2*time.Minute, 24*time.Hour))
	defer ts.Close()

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "typing-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "typing-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "typing indicator",
		"max_turns":   1,
		"ttl_seconds": 600,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")
	humanCode := mustString(t, body, "human_code")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/viewers", map[string]any{
		"op":         "join",
		"human_code": humanCode,
	}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("viewer join status=%d body=%v", resp.StatusCode, body)
	}
	viewerToken := mustString(t, body, "viewer_token")

	staleAt := time.Now().UTC().Add(-30 * time.Second)
	if _, err := db.ExecContext(context.Background(), `UPDATE room_viewers SET last_heartbeat_at = $1 WHERE viewer_token = $2`, staleAt, viewerToken); err != nil {
		t.Fatalf("stale viewer heartbeat: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/rooms/"+roomID+"/viewer-events", nil)
	if err != nil {
		t.Fatalf("new viewer stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+viewerToken)

	sseResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("open viewer stream: %v", err)
	}
	defer sseResp.Body.Close()

	if sseResp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(sseResp.Body)
		t.Fatalf("viewer stream status=%d body=%s", sseResp.StatusCode, string(body))
	}

	var payload map[string]any
	if err := json.NewDecoder(sseResp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode viewer stream error payload: %v", err)
	}
	if got, _ := payload["error"].(string); got != "viewer_not_found" {
		t.Fatalf("viewer stream error=%v body=%v", payload["error"], payload)
	}
}

func TestSQLModeRoomAccessTokenFlow(t *testing.T) {
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

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "rat-a"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register a status=%d body=%v", resp.StatusCode, body)
	}
	apiA := mustString(t, body, "api_key")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "rat-b"}, "")
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
		"topic":       "room access token flow",
		"max_turns":   6,
		"ttl_seconds": 600,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")
	roomID := mustString(t, body, "room_id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/access-token", nil, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create room access token status=%d body=%v", resp.StatusCode, body)
	}
	roomToken := mustString(t, body, "token")
	if got := mustString(t, body, "scope"); got != "room:automation" {
		t.Fatalf("room access token scope=%s want=room:automation", got)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/state", nil, roomToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("room state with room token status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, roomToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("room context with room token status=%d body=%v", resp.StatusCode, body)
	}
	bundleHash := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/events/history", nil, roomToken)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("room events history with room token status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "message via room token",
		"bundle_hash":   bundleHash,
	}, roomToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("room message with room token status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/close", nil, roomToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("room close with room token status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/state", nil, roomToken)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("room state with revoked room token status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/access-token", nil, tokenA)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("create room access token after close status=%d body=%v", resp.StatusCode, body)
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

func TestSQLModeAgentStreamSSEAndAckResume(t *testing.T) {
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

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "stream-live-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "stream-live-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "agent-stream-live",
		"max_turns":   6,
		"ttl_seconds": 600,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect listing status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")

	streamAResp, streamAReader := openAgentStream(t, ts, tokenA, "")
	defer streamAResp.Body.Close()
	eventsA, errsA := startGenericSSEStream(streamAReader)
	helloA := waitForGenericSSEEventType(t, eventsA, errsA, "stream.hello", 5*time.Second)
	if got, _ := helloA.Payload["resume_status"].(string); got != "fresh" {
		t.Fatalf("hello resume_status=%q want=fresh payload=%v", got, helloA.Payload)
	}
	turnReadyA := waitForGenericSSEEventType(t, eventsA, errsA, "room.turn_ready", 5*time.Second)
	deliveryA := mustStringMap(t, turnReadyA.Payload, "delivery_id")
	if got := mustStringMap(t, turnReadyA.Payload, "room_id"); got != roomID {
		t.Fatalf("turn_ready room_id=%q want=%q payload=%v", got, roomID, turnReadyA.Payload)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/stream/ack", map[string]any{
		"delivery_id": deliveryA,
	}, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack a status=%d body=%v", resp.StatusCode, body)
	}

	_ = streamAResp.Body.Close()
	streamAResumeResp, streamAResumeReader := openAgentStream(t, ts, tokenA, deliveryA)
	defer streamAResumeResp.Body.Close()
	eventsAResume, errsAResume := startGenericSSEStream(streamAResumeReader)
	helloAResume := waitForGenericSSEEventType(t, eventsAResume, errsAResume, "stream.hello", 5*time.Second)
	if got, _ := helloAResume.Payload["resume_status"].(string); got != "ok" {
		t.Fatalf("resume hello status=%q want=ok payload=%v", got, helloAResume.Payload)
	}
	expectNoGenericSSEEvent(t, eventsAResume, errsAResume, 1200*time.Millisecond)

	streamBResp, streamBReader := openAgentStream(t, ts, tokenB, "")
	defer streamBResp.Body.Close()
	eventsB, errsB := startGenericSSEStream(streamBReader)
	helloB := waitForGenericSSEEventType(t, eventsB, errsB, "stream.hello", 5*time.Second)
	if got, _ := helloB.Payload["resume_status"].(string); got != "fresh" {
		t.Fatalf("hello b resume_status=%q want=fresh payload=%v", got, helloB.Payload)
	}
	expectNoGenericSSEEvent(t, eventsB, errsB, 1200*time.Millisecond)

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context a status=%d body=%v", resp.StatusCode, body)
	}
	bundleA := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "agent-stream-message-a",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a status=%d body=%v", resp.StatusCode, body)
	}

	turnReadyB := waitForGenericSSEEventType(t, eventsB, errsB, "room.turn_ready", 5*time.Second)
	deliveryB := mustStringMap(t, turnReadyB.Payload, "delivery_id")
	if got := mustStringMap(t, turnReadyB.Payload, "room_id"); got != roomID {
		t.Fatalf("turn_ready b room_id=%q want=%q payload=%v", got, roomID, turnReadyB.Payload)
	}

	_ = streamBResp.Body.Close()
	streamBUnackedCursorResp, streamBUnackedCursorReader := openAgentStream(t, ts, tokenB, deliveryB)
	defer streamBUnackedCursorResp.Body.Close()
	eventsBUnackedCursor, errsBUnackedCursor := startGenericSSEStream(streamBUnackedCursorReader)
	_ = waitForGenericSSEEventType(t, eventsBUnackedCursor, errsBUnackedCursor, "stream.hello", 5*time.Second)
	replayRequiredForUnacked := waitForGenericSSEEventType(t, eventsBUnackedCursor, errsBUnackedCursor, "stream.replay_required", 5*time.Second)
	if got, _ := replayRequiredForUnacked.Payload["type"].(string); got != "stream.replay_required" {
		t.Fatalf("replay_required for unacked cursor payload=%v", replayRequiredForUnacked.Payload)
	}

	streamBReplayResp, streamBReplayReader := openAgentStream(t, ts, tokenB, "")
	defer streamBReplayResp.Body.Close()
	eventsBReplay, errsBReplay := startGenericSSEStream(streamBReplayReader)
	_ = waitForGenericSSEEventType(t, eventsBReplay, errsBReplay, "stream.hello", 5*time.Second)
	turnReadyBReplay := waitForGenericSSEEventType(t, eventsBReplay, errsBReplay, "room.turn_ready", 5*time.Second)
	if got := mustStringMap(t, turnReadyBReplay.Payload, "delivery_id"); got != deliveryB {
		t.Fatalf("replayed delivery_id=%q want=%q payload=%v", got, deliveryB, turnReadyBReplay.Payload)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/stream/ack", map[string]any{
		"delivery_id": deliveryB,
	}, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack b status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/close", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close room status=%d body=%v", resp.StatusCode, body)
	}
	closedB := waitForGenericSSEEventType(t, eventsBReplay, errsBReplay, "room.closed", 5*time.Second)
	if got := mustStringMap(t, closedB.Payload, "room_id"); got != roomID {
		t.Fatalf("closed room_id=%q want=%q payload=%v", got, roomID, closedB.Payload)
	}

	streamReplayRequiredResp, streamReplayRequiredReader := openAgentStream(t, ts, tokenA, "missing_delivery")
	defer streamReplayRequiredResp.Body.Close()
	eventsReplayRequired, errsReplayRequired := startGenericSSEStream(streamReplayRequiredReader)
	_ = waitForGenericSSEEventType(t, eventsReplayRequired, errsReplayRequired, "stream.hello", 5*time.Second)
	replayRequired := waitForGenericSSEEventType(t, eventsReplayRequired, errsReplayRequired, "stream.replay_required", 5*time.Second)
	if got, _ := replayRequired.Payload["type"].(string); got != "stream.replay_required" {
		t.Fatalf("replay_required payload=%v", replayRequired.Payload)
	}
}

func TestSQLModeRoomEventsStreamReplaysFullBacklogBeforeLive(t *testing.T) {
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

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "backlog-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "backlog-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "room-backlog",
		"max_turns":   8,
		"ttl_seconds": 600,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect listing status=%d body=%v", resp.StatusCode, body)
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

	for i := 0; i < 205; i++ {
		if _, err := store.AppendRoomEvent(context.Background(), repository.AppendRoomEventInput{
			RoomID:    roomID,
			EventType: "backlog.event",
		}); err != nil {
			t.Fatalf("append backlog room event %d: %v", i, err)
		}
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

	reader := bufio.NewReader(sseResp.Body)
	eventCh, errCh := startSSEEventStream(reader)
	count := 0
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for count < 205 {
		select {
		case err := <-errCh:
			t.Fatalf("sse stream error: %v", err)
		case ev := <-eventCh:
			if ev.Type == "backlog.event" {
				count++
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for backlog replay count=%d", count)
		}
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

type genericSSEEnvelope struct {
	EventID string
	Type    string
	Payload map[string]any
}

func openAgentStream(t *testing.T, ts *httptest.Server, token, lastDeliveryID string) (*http.Response, *bufio.Reader) {
	t.Helper()
	url := ts.URL + "/v1/agent/stream"
	if strings.TrimSpace(lastDeliveryID) != "" {
		url += "?last_delivery_id=" + lastDeliveryID
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new agent stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("open agent stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("agent stream status=%d body=%s", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("agent stream content-type=%q", got)
	}
	return resp, bufio.NewReader(resp.Body)
}

func startGenericSSEStream(reader *bufio.Reader) (<-chan genericSSEEnvelope, <-chan error) {
	events := make(chan genericSSEEnvelope, 32)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		for {
			ev, err := readGenericSSEFrame(reader)
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

func waitForGenericSSEEventType(t *testing.T, eventCh <-chan genericSSEEnvelope, errCh <-chan error, eventType string, timeout time.Duration) genericSSEEnvelope {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case err := <-errCh:
			t.Fatalf("agent stream error: %v", err)
		case ev, ok := <-eventCh:
			if !ok {
				t.Fatalf("agent stream closed while waiting for event type %q", eventType)
			}
			if ev.Type == eventType {
				return ev
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for agent stream event type %q", eventType)
		}
	}
}

func expectNoGenericSSEEvent(t *testing.T, eventCh <-chan genericSSEEnvelope, errCh <-chan error, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-errCh:
		t.Fatalf("agent stream error while expecting silence: %v", err)
	case ev, ok := <-eventCh:
		if !ok {
			return
		}
		t.Fatalf("unexpected agent stream event type=%q id=%s", ev.Type, ev.EventID)
	case <-timer.C:
	}
}

func waitForGenericSSEStreamClose(t *testing.T, eventCh <-chan genericSSEEnvelope, errCh <-chan error, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case err := <-errCh:
			t.Fatalf("agent stream error while waiting for close: %v", err)
		case _, ok := <-eventCh:
			if !ok {
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for agent stream to close")
		}
	}
}

func readGenericSSEFrame(reader *bufio.Reader) (genericSSEEnvelope, error) {
	for {
		var (
			eventID   string
			eventType string
			dataBuf   strings.Builder
		)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return genericSSEEnvelope{}, err
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if eventType == "" || dataBuf.Len() == 0 {
					break
				}
				payload := map[string]any{}
				if err := json.Unmarshal([]byte(dataBuf.String()), &payload); err != nil {
					return genericSSEEnvelope{}, fmt.Errorf("unmarshal generic data: %w", err)
				}
				return genericSSEEnvelope{
					EventID: eventID,
					Type:    eventType,
					Payload: payload,
				}, nil
			}
			if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "retry:") {
				continue
			}
			if strings.HasPrefix(line, "id:") {
				eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
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

func TestSQLModeAgentActionableRoomsEndpoint(t *testing.T) {
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

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "stream-a"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register a status=%d body=%v", resp.StatusCode, body)
	}
	apiA := mustString(t, body, "api_key")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "stream-b"}, "")
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
		"topic":       "agent stream recovery",
		"max_turns":   4,
		"ttl_seconds": 600,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	listingID := mustString(t, body, "id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect listing status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/agent/actionable-rooms", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("actionable rooms a status=%d body=%v", resp.StatusCode, body)
	}
	actionableA, ok := body["actionable"].([]any)
	if !ok || len(actionableA) != 1 {
		t.Fatalf("actionable rooms a payload=%v", body)
	}
	itemA, ok := actionableA[0].(map[string]any)
	if !ok {
		t.Fatalf("actionable room a payload=%v", actionableA[0])
	}
	if got, _ := itemA["room_id"].(string); got != roomID {
		t.Fatalf("actionable room id=%q want=%q", got, roomID)
	}
	if got, _ := itemA["next_actor_id"].(string); got == "" {
		t.Fatalf("next_actor_id missing: %v", itemA)
	}
	if got, _ := itemA["token"].(string); got == "" {
		t.Fatalf("token missing: %v", itemA)
	}
	if terminal, ok := body["terminal"].([]any); !ok || len(terminal) != 0 {
		t.Fatalf("unexpected terminal payload=%v", body["terminal"])
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/context", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("context a status=%d body=%v", resp.StatusCode, body)
	}
	bundleA := mustString(t, body, "bundle_hash")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "stream-msg-a",
		"bundle_hash":   bundleA,
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/agent/actionable-rooms", nil, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("actionable rooms b status=%d body=%v", resp.StatusCode, body)
	}
	actionableB, ok := body["actionable"].([]any)
	if !ok || len(actionableB) != 1 {
		t.Fatalf("actionable rooms b payload=%v", body)
	}
	itemB, ok := actionableB[0].(map[string]any)
	if !ok {
		t.Fatalf("actionable room b payload=%v", actionableB[0])
	}
	if got, _ := itemB["room_id"].(string); got != roomID {
		t.Fatalf("actionable room b id=%q want=%q", got, roomID)
	}
	if got, _ := itemB["token"].(string); got == "" {
		t.Fatalf("token missing for b: %v", itemB)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/close", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close room status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/agent/actionable-rooms", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("actionable rooms after close status=%d body=%v", resp.StatusCode, body)
	}
	if actionable, ok := body["actionable"].([]any); !ok || len(actionable) != 0 {
		t.Fatalf("unexpected actionable after close=%v", body["actionable"])
	}
	terminalA, ok := body["terminal"].([]any)
	if !ok || len(terminalA) == 0 {
		t.Fatalf("terminal rooms a payload=%v", body)
	}

	var turnReadyCount, closedCount int
	rows, err := db.Query(`SELECT agent_id, type FROM agent_stream_deliveries WHERE room_id = $1 ORDER BY seq ASC`, roomID)
	if err != nil {
		t.Fatalf("query stream deliveries: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var agentID, typ string
		if err := rows.Scan(&agentID, &typ); err != nil {
			t.Fatalf("scan stream delivery: %v", err)
		}
		switch typ {
		case "room.turn_ready":
			turnReadyCount++
		case "room.closed":
			closedCount++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stream deliveries: %v", err)
	}
	if turnReadyCount < 2 {
		t.Fatalf("room.turn_ready count=%d want>=2", turnReadyCount)
	}
	if closedCount != 2 {
		t.Fatalf("room.closed count=%d want=2", closedCount)
	}
}

func mustStringMap(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	got, ok := m[key].(string)
	if !ok || got == "" {
		t.Fatalf("missing string key %q in %v", key, m)
	}
	return got
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
	up4, err := os.ReadFile(filepath.Join(migDir, "000004_api_request_logs.up.sql"))
	if err != nil {
		t.Fatalf("read api request logs up migration: %v", err)
	}
	up5, err := os.ReadFile(filepath.Join(migDir, "000005_owner_first_listing_flow.up.sql"))
	if err != nil {
		t.Fatalf("read owner-first listing flow up migration: %v", err)
	}
	up6, err := os.ReadFile(filepath.Join(migDir, "000006_webhook_foundation.up.sql"))
	if err != nil {
		t.Fatalf("read webhook foundation up migration: %v", err)
	}
	up7, err := os.ReadFile(filepath.Join(migDir, "000007_webhook_endpoint_delete_cascade.up.sql"))
	if err != nil {
		t.Fatalf("read webhook endpoint delete cascade up migration: %v", err)
	}
	up8, err := os.ReadFile(filepath.Join(migDir, "000008_agent_stream_deliveries.up.sql"))
	if err != nil {
		t.Fatalf("read agent stream deliveries up migration: %v", err)
	}
	up9, err := os.ReadFile(filepath.Join(migDir, "000009_human_code_ttl.up.sql"))
	if err != nil {
		t.Fatalf("read human code ttl up migration: %v", err)
	}
	up10, err := os.ReadFile(filepath.Join(migDir, "000010_stream_coordination.up.sql"))
	if err != nil {
		t.Fatalf("read stream coordination up migration: %v", err)
	}
	up11, err := os.ReadFile(filepath.Join(migDir, "000011_human_code_ttl_backfill.up.sql"))
	if err != nil {
		t.Fatalf("read human code ttl backfill up migration: %v", err)
	}
	up12, err := os.ReadFile(filepath.Join(migDir, "000012_agent_policy_state.up.sql"))
	if err != nil {
		t.Fatalf("read agent policy state up migration: %v", err)
	}
	up13, err := os.ReadFile(filepath.Join(migDir, "000013_room_message_key.up.sql"))
	if err != nil {
		t.Fatalf("read room message key up migration: %v", err)
	}
	up14, err := os.ReadFile(filepath.Join(migDir, "000014_room_topic.up.sql"))
	if err != nil {
		t.Fatalf("read room topic up migration: %v", err)
	}
	up15, err := os.ReadFile(filepath.Join(migDir, "000015_api_request_logs_route_name.up.sql"))
	if err != nil {
		t.Fatalf("read api request logs route name up migration: %v", err)
	}

	if _, err := db.Exec(string(down)); err != nil {
		t.Fatalf("exec down migration: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_stream_deliveries`); err != nil {
		t.Fatalf("cleanup agent stream deliveries: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_event_stream_open_events`); err != nil {
		t.Fatalf("cleanup room event stream open events: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_event_stream_leases`); err != nil {
		t.Fatalf("cleanup room event stream leases: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_policy_state`); err != nil {
		t.Fatalf("cleanup agent policy state: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_scoped_tokens`); err != nil {
		t.Fatalf("cleanup room scoped tokens: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS webhook_outbox`); err != nil {
		t.Fatalf("cleanup webhook outbox: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_webhook_endpoints`); err != nil {
		t.Fatalf("cleanup agent webhook endpoints: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS api_request_logs`); err != nil {
		t.Fatalf("cleanup api request logs: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS room_context_state`); err != nil {
		t.Fatalf("cleanup room context state: %v", err)
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
	if _, err := db.Exec(string(up4)); err != nil {
		t.Fatalf("exec api request logs up migration: %v", err)
	}
	if _, err := db.Exec(string(up5)); err != nil {
		t.Fatalf("exec owner-first listing flow up migration: %v", err)
	}
	if _, err := db.Exec(string(up6)); err != nil {
		t.Fatalf("exec webhook foundation up migration: %v", err)
	}
	if _, err := db.Exec(string(up7)); err != nil {
		t.Fatalf("exec webhook endpoint delete cascade up migration: %v", err)
	}
	if _, err := db.Exec(string(up8)); err != nil {
		t.Fatalf("exec agent stream deliveries up migration: %v", err)
	}
	if _, err := db.Exec(string(up9)); err != nil {
		t.Fatalf("exec human code ttl up migration: %v", err)
	}
	if _, err := db.Exec(string(up10)); err != nil {
		t.Fatalf("exec stream coordination up migration: %v", err)
	}
	if _, err := db.Exec(string(up11)); err != nil {
		t.Fatalf("exec human code ttl backfill up migration: %v", err)
	}
	if _, err := db.Exec(string(up12)); err != nil {
		t.Fatalf("exec agent policy state up migration: %v", err)
	}
	if _, err := db.Exec(string(up13)); err != nil {
		t.Fatalf("exec room message key up migration: %v", err)
	}
	if _, err := db.Exec(string(up14)); err != nil {
		t.Fatalf("exec room topic up migration: %v", err)
	}
	if _, err := db.Exec(string(up15)); err != nil {
		t.Fatalf("exec api request logs route name up migration: %v", err)
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
