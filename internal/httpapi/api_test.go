package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func doJSON(t *testing.T, ts *httptest.Server, method, path string, body any, bearer string) (*http.Response, map[string]any) {
	t.Helper()

	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}

	req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}

	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	_ = resp.Body.Close()
	return resp, decoded
}

func mustString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing key: %s", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		t.Fatalf("key %s is not non-empty string", key)
	}
	return s
}

func mustPayloadString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing payload key: %s", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		t.Fatalf("payload key %s is not non-empty string", key)
	}
	return s
}

func TestListingConnectAndSequentialMessagingFlow(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(NewRouter())
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
		"topic":       "go test room",
		"tags":        []string{"go", "mvp"},
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
	if got, _ := body["room_state"].(string); got != string("OPEN") {
		t.Fatalf("create listing room_state=%v body=%v", body["room_state"], body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "before-connect",
	}, tokenA)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("message before connect status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["room_state"].(string); got != string("ACTIVE") {
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

	resp, body = doJSON(t, ts, http.MethodGet, "/v1/rooms/"+roomID+"/state", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state status=%d body=%v", resp.StatusCode, body)
	}
	if _, ok := body["next_turn"]; !ok {
		t.Fatalf("state missing next_turn: %v", body)
	}
	if _, ok := body["next_actor_id"]; !ok {
		t.Fatalf("state missing next_actor_id: %v", body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 0,
		"ciphertext":    "c1",
	}, tokenA)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message a turn0 status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 1,
		"ciphertext":    "bad-turn",
	}, tokenA)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("message a wrong-turn status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 1,
		"ciphertext":    "c2",
	}, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("message b turn1 status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/close", nil, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("close status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/messages", map[string]any{
		"expected_turn": 2,
		"ciphertext":    "after-close",
	}, tokenA)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("message after close status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/transcript", map[string]any{"human_code": "wrong"}, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("transcript wrong code status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/transcript", map[string]any{"human_code": humanCode}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript good code status=%d body=%v", resp.StatusCode, body)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("unexpected transcript messages=%v", body["messages"])
	}
	if got, _ := body["room_topic"].(string); got != "go test room" {
		t.Fatalf("room_topic=%q want=%q body=%v", got, "go test room", body)
	}
	if got, _ := body["agent_a_id"].(string); got == "" {
		t.Fatalf("agent_a_id missing in transcript body=%v", body)
	}
	if got, _ := body["agent_b_id"].(string); got == "" {
		t.Fatalf("agent_b_id missing in transcript body=%v", body)
	}
	if got, _ := body["turn_index"].(float64); got != 2 {
		t.Fatalf("turn_index=%v want=2 body=%v", got, body)
	}
	if _, ok := body["next_actor_id"]; !ok {
		t.Fatalf("next_actor_id missing in transcript body=%v", body)
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected first message payload=%v", msgs[0])
	}
	if _, ok := first["sender_name"].(string); !ok {
		t.Fatalf("sender_name missing in transcript message=%v", first)
	}
}

func TestModeEndpointInMemory(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	resp, body := doJSON(t, ts, http.MethodGet, "/v1/mode", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["mode"].(string); got != "polling" {
		t.Fatalf("mode=%v want=polling body=%v", body["mode"], body)
	}
	if _, ok := body["poll_interval_ms"]; !ok {
		t.Fatalf("mode missing poll_interval_ms: %v", body)
	}
}

func TestRoomLeaveEndpointUnsupported(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "agent-a"}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status=%d body=%v", resp.StatusCode, body)
	}
	apiKey := mustString(t, body, "api_key")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiKey}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d body=%v", resp.StatusCode, body)
	}
	token := mustString(t, body, "session_token")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "leave-test",
		"max_turns":   4,
		"ttl_seconds": 300,
	}, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create listing status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/leave", nil, token)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("leave status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["error"].(string); got != "endpoint_not_supported" {
		t.Fatalf("leave error=%v body=%v", body["error"], body)
	}
}

func TestAuthRequiredForListingCreate(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic": "needs-auth",
	}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
}

func TestViewerJoinHeartbeatLeave(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "agent-a"}, "")
	apiA := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "agent-b"}, "")
	apiB := mustString(t, body, "api_key")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiA}, "")
	tokenA := mustString(t, body, "session_token")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiB}, "")
	tokenB := mustString(t, body, "session_token")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "viewer-policy",
		"max_turns":   4,
		"ttl_seconds": 300,
	}, tokenA)
	listingID := mustString(t, body, "id")
	humanCode := mustString(t, body, "human_code")

	resp, body := doJSON(t, ts, http.MethodPost, "/v1/listings/"+listingID+"/connect", nil, tokenB)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect status=%d body=%v", resp.StatusCode, body)
	}
	roomID := mustString(t, body, "room_id")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/viewers", map[string]any{
		"op":         "join",
		"human_code": "wrong",
	}, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer join wrong code status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/viewers", map[string]any{
		"op":         "join",
		"human_code": humanCode,
	}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("viewer join status=%d body=%v", resp.StatusCode, body)
	}
	viewerToken := mustString(t, body, "viewer_token")

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/viewers", map[string]any{
		"op":           "heartbeat",
		"viewer_token": viewerToken,
	}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer heartbeat status=%d body=%v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/v1/rooms/"+roomID+"/viewers", map[string]any{
		"op":           "leave",
		"viewer_token": viewerToken,
	}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer leave status=%d body=%v", resp.StatusCode, body)
	}
}

func TestTranscriptRejectsHumanCodeInQuery(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	_, body := doJSON(t, ts, http.MethodPost, "/v1/agent/register", map[string]any{"name": "query-agent"}, "")
	apiKey := mustString(t, body, "api_key")
	_, body = doJSON(t, ts, http.MethodPost, "/v1/agent/login", map[string]any{"api_key": apiKey}, "")
	token := mustString(t, body, "session_token")

	_, body = doJSON(t, ts, http.MethodPost, "/v1/listings", map[string]any{
		"topic":       "query-reject",
		"max_turns":   4,
		"ttl_seconds": 300,
	}, token)
	roomID := mustString(t, body, "room_id")
	humanCode := mustString(t, body, "human_code")

	resp, body := doJSON(
		t,
		ts,
		http.MethodPost,
		"/v1/rooms/"+roomID+"/transcript?human_code="+humanCode,
		map[string]any{"human_code": humanCode},
		"",
	)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if got, _ := body["error"].(string); got != "invalid_request" {
		t.Fatalf("error=%v body=%v", body["error"], body)
	}
}

func TestAuthAgentIDRejectsExpiredSession(t *testing.T) {
	t.Parallel()

	a := newApp(options{})
	baseNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return baseNow }

	a.sessions["as_expired"] = authSession{
		AgentID:   "agt_test",
		ExpiresAt: baseNow.Add(-1 * time.Second),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/listings", nil)
	req.Header.Set("Authorization", "Bearer as_expired")

	agentID, ok := a.authAgentID(req)
	if ok || agentID != "" {
		t.Fatalf("expected expired session rejected, got ok=%v agentID=%q", ok, agentID)
	}
	if _, stillExists := a.sessions["as_expired"]; stillExists {
		t.Fatal("expected expired session removed from session map")
	}
}
