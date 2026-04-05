package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/repository"
	"github.com/febrian/areyouai/internal/security"
	"github.com/febrian/areyouai/internal/service/a2a"
)

type fakeLeaseStore struct {
	acquireOut repository.AcquireRoomEventStreamLeaseResult
	acquireErr error
	releaseErr error
	acquired   int
	released   int
	lastIn     repository.AcquireRoomEventStreamLeaseInput
}

func (f *fakeLeaseStore) AcquireRoomEventStreamLease(_ context.Context, in repository.AcquireRoomEventStreamLeaseInput) (repository.AcquireRoomEventStreamLeaseResult, error) {
	f.acquired++
	f.lastIn = in
	return f.acquireOut, f.acquireErr
}

func (f *fakeLeaseStore) ReleaseRoomEventStreamLease(_ context.Context, _ string) error {
	f.released++
	return f.releaseErr
}

func TestAllowIPMessageRateLimit(t *testing.T) {
	t.Parallel()

	h := &sqlHTTP{ipWindows: map[string][]time.Time{}}
	now := time.Now().UTC()
	addr := "127.0.0.1:12345"

	for i := 0; i < 120; i++ {
		if !h.allowIPMessage(addr, now) {
			t.Fatalf("unexpected rate limit at request %d", i+1)
		}
	}
	if h.allowIPMessage(addr, now) {
		t.Fatal("expected ip rate limit on 121st request")
	}
}

func TestRemoteIP(t *testing.T) {
	t.Parallel()

	if got := remoteIP("10.0.0.1:8080"); got != "10.0.0.1" {
		t.Fatalf("remoteIP split got=%q", got)
	}
	if got := remoteIP("10.0.0.2"); got != "10.0.0.2" {
		t.Fatalf("remoteIP raw got=%q", got)
	}
}

func TestWriteServiceErrPolicyBlocked(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeServiceErr(w, a2a.ErrPolicyBlocked)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=%d", w.Code, http.StatusForbidden)
	}
}

func TestWriteServiceErrPayloadTooLarge(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeServiceErr(w, a2a.ErrPayloadTooLarge)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want=%d", w.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(w.Body.String(), `"error":"payload_too_large"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"max_chars":`) || !strings.Contains(w.Body.String(), `8192`) {
		t.Fatalf("missing max_chars in body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), strconv.Itoa(security.MaxPersistMessageChars)) {
		t.Fatalf("missing limit in body=%s", w.Body.String())
	}
}

func TestWriteServiceErrTurnMismatch(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeServiceErr(w, a2a.ErrTurnMismatch)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d", w.Code, http.StatusConflict)
	}
	if !strings.Contains(w.Body.String(), `"error":"turn_mismatch"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestWriteServiceErrStaleBundleHash(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeServiceErr(w, a2a.ErrStaleBundleHash)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d", w.Code, http.StatusConflict)
	}
	if !strings.Contains(w.Body.String(), `"error":"stale_bundle_hash"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestWriteServiceErrRoomNotActive(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeServiceErr(w, a2a.ErrRoomNotActive)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d", w.Code, http.StatusConflict)
	}
	if !strings.Contains(w.Body.String(), `"error":"room_not_active"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestAdminAuthorized(t *testing.T) {
	t.Parallel()

	h := &sqlHTTP{adminToken: "adm_secret"}

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/overview", nil)
	req.Header.Set("Authorization", "Bearer adm_secret")
	if !h.adminAuthorized(req) {
		t.Fatal("expected admin auth success with bearer token")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/admin/overview", nil)
	req2.Header.Set("X-Admin-Token", "adm_secret")
	if h.adminAuthorized(req2) {
		t.Fatal("expected admin auth failure with legacy X-Admin-Token")
	}

	req3 := httptest.NewRequest(http.MethodGet, "/v1/admin/overview", nil)
	req3.Header.Set("Authorization", "Bearer wrong")
	if h.adminAuthorized(req3) {
		t.Fatal("expected admin auth failure for wrong bearer token")
	}
}

func TestHandleAdminRejectsLegacyXAdminToken(t *testing.T) {
	t.Parallel()

	h := &sqlHTTP{adminToken: "adm_secret"}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/overview", nil)
	req.Header.Set("X-Admin-Token", "adm_secret")
	w := httptest.NewRecorder()

	h.handleAdmin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"error":"invalid_request"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestHandleAdminRejectsAdminTokenInQuery(t *testing.T) {
	t.Parallel()

	h := &sqlHTTP{adminToken: "adm_secret"}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/overview?admin_token=adm_secret", nil)
	w := httptest.NewRecorder()

	h.handleAdmin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"error":"invalid_request"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestParseEventHistoryQuery(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/room_1/events/history", nil)
	since, limit, err := parseEventHistoryQuery(req)
	if err != nil {
		t.Fatalf("default parse error: %v", err)
	}
	if since != 0 {
		t.Fatalf("default since=%d want=0", since)
	}
	if limit != 200 {
		t.Fatalf("default limit=%d want=200", limit)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/room_1/events/history?since=12&limit=25", nil)
	since, limit, err = parseEventHistoryQuery(req)
	if err != nil {
		t.Fatalf("parse with values error: %v", err)
	}
	if since != 12 {
		t.Fatalf("since=%d want=12", since)
	}
	if limit != 25 {
		t.Fatalf("limit=%d want=25", limit)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/room_1/events/history?since=-1", nil)
	if _, _, err = parseEventHistoryQuery(req); err == nil {
		t.Fatal("expected error for negative since")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/room_1/events/history?since=abc", nil)
	if _, _, err = parseEventHistoryQuery(req); err == nil {
		t.Fatal("expected error for invalid since")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/room_1/events/history?limit=0", nil)
	if _, _, err = parseEventHistoryQuery(req); err == nil {
		t.Fatal("expected error for invalid limit")
	}
}

func TestParseEventStreamQuery(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/room_1/events", nil)
	req.Header.Set("Last-Event-ID", "42")
	since, limit, err := parseEventStreamQuery(req)
	if err != nil {
		t.Fatalf("parse stream header error: %v", err)
	}
	if since != 42 {
		t.Fatalf("since=%d want=42", since)
	}
	if limit != 200 {
		t.Fatalf("limit=%d want=200", limit)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/room_1/events?since=7", nil)
	req.Header.Set("Last-Event-ID", "42")
	since, _, err = parseEventStreamQuery(req)
	if err != nil {
		t.Fatalf("parse stream precedence error: %v", err)
	}
	if since != 7 {
		t.Fatalf("query since must override header, got=%d", since)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/room_1/events", nil)
	req.Header.Set("Last-Event-ID", "bad")
	if _, _, err = parseEventStreamQuery(req); err == nil {
		t.Fatal("expected error for invalid Last-Event-ID")
	}
}

func TestAcquireStreamSlotMaxActivePerAgentRoom(t *testing.T) {
	t.Parallel()

	h := &sqlHTTP{}
	now := time.Now().UTC()
	releases := make([]func(), 0, maxActiveStreamsPerAgentRoom)
	for i := 0; i < maxActiveStreamsPerAgentRoom; i++ {
		release, _, err := h.acquireStreamSlot(context.Background(), "agt_a", "room_x", "127.0.0.1:1234", now)
		if err != nil {
			t.Fatalf("unexpected acquire error at %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	if _, reason, err := h.acquireStreamSlot(context.Background(), "agt_a", "room_x", "127.0.0.1:1234", now); !errors.Is(err, a2a.ErrRateLimit) {
		t.Fatalf("expected rate limit err, got reason=%q err=%v", reason, err)
	}
	for _, release := range releases {
		release()
	}
}

func TestAcquireStreamSlotReconnectFloodPerRoomAgent(t *testing.T) {
	t.Parallel()

	h := &sqlHTTP{}
	now := time.Now().UTC()
	for i := 0; i < maxStreamConnectsPerMinuteRoomKey; i++ {
		release, _, err := h.acquireStreamSlot(context.Background(), "agt_flood", "room_flood", "127.0.0.1:5555", now)
		if err != nil {
			t.Fatalf("unexpected acquire error at %d: %v", i, err)
		}
		release()
	}
	if _, reason, err := h.acquireStreamSlot(context.Background(), "agt_flood", "room_flood", "127.0.0.1:5555", now); !errors.Is(err, a2a.ErrRateLimit) {
		t.Fatalf("expected flood rate limit err, got reason=%q err=%v", reason, err)
	}
}

func TestAcquireStreamSlotReconnectFloodPerIP(t *testing.T) {
	t.Parallel()

	h := &sqlHTTP{}
	now := time.Now().UTC()
	for i := 0; i < maxStreamConnectsPerMinuteIP; i++ {
		agentID := "agt_" + string(rune('a'+(i%26)))
		release, _, err := h.acquireStreamSlot(context.Background(), agentID, "room_"+string(rune('a'+(i%26))), "10.0.0.1:9999", now)
		if err != nil {
			t.Fatalf("unexpected acquire error at %d: %v", i, err)
		}
		release()
	}
	if _, reason, err := h.acquireStreamSlot(context.Background(), "agt_final", "room_final", "10.0.0.1:9999", now); !errors.Is(err, a2a.ErrRateLimit) {
		t.Fatalf("expected ip flood rate limit err, got reason=%q err=%v", reason, err)
	}
}

func TestHandleTranscriptRejectsHumanCodeInQuery(t *testing.T) {
	t.Parallel()

	h := &sqlHTTP{}
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms/room_x/transcript?human_code=hc_x", strings.NewReader(`{"human_code":"hc_x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.handleTranscript(w, req, "room_x")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"error":"invalid_request"`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestAcquireStreamSlotUsesDistributedLeaseStore(t *testing.T) {
	t.Parallel()

	leases := &fakeLeaseStore{
		acquireOut: repository.AcquireRoomEventStreamLeaseResult{Acquired: true},
	}
	h := &sqlHTTP{streamLeaseStore: leases}
	release, reason, err := h.acquireStreamSlot(context.Background(), "agt_dist", "room_dist", "127.0.0.1:3456", time.Now().UTC())
	if err != nil {
		t.Fatalf("acquire stream slot: %v", err)
	}
	if reason != "" {
		t.Fatalf("deny reason=%q want empty", reason)
	}
	if leases.acquired != 1 {
		t.Fatalf("acquired calls=%d want=1", leases.acquired)
	}
	if leases.lastIn.RoomID != "room_dist" || leases.lastIn.AgentID != "agt_dist" {
		t.Fatalf("unexpected acquire input: %+v", leases.lastIn)
	}
	release()
	if leases.released != 1 {
		t.Fatalf("released calls=%d want=1", leases.released)
	}
}
