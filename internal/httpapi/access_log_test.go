package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWithAccessLogsPreservesFlusherForSSE(t *testing.T) {
	t.Parallel()

	sseHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "retry: 3000\n\n")
		flusher.Flush()
	})

	handler := withAccessLogs(sseHandler, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/room_test/events?since=0&limit=10", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.StatusCode, string(body))
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type=%q", got)
	}
	if !strings.Contains(string(body), "retry: 3000") {
		t.Fatalf("missing sse retry frame body=%q", string(body))
	}
	if got := strings.TrimSpace(res.Header.Get("X-Request-Id")); got == "" {
		t.Fatal("missing x-request-id header")
	}
}

func TestSanitizeQueryStringRedactsSensitiveKeys(t *testing.T) {
	t.Parallel()

	out := sanitizeQueryString("since=12&human_code=hc_secret&token=as_secret&q=safe")
	parsed, err := url.ParseQuery(out)
	if err != nil {
		t.Fatalf("parse sanitized query: %v", err)
	}

	if got := parsed.Get("since"); got != "12" {
		t.Fatalf("since=%q want=12", got)
	}
	if got := parsed.Get("q"); got != "safe" {
		t.Fatalf("q=%q want=safe", got)
	}
	if got := parsed.Get("human_code"); got != "[REDACTED]" {
		t.Fatalf("human_code=%q want=[REDACTED]", got)
	}
	if got := parsed.Get("token"); got != "[REDACTED]" {
		t.Fatalf("token=%q want=[REDACTED]", got)
	}
}

func TestRequestRouteNameSeparatesContextAndAck(t *testing.T) {
	t.Parallel()

	if got := requestRouteName(http.MethodGet, "/v1/rooms/room_1/context"); got != "room_context" {
		t.Fatalf("GET /context route=%q want room_context", got)
	}
	if got := requestRouteName(http.MethodPost, "/v1/rooms/room_1/context/ack"); got != "room_context_ack" {
		t.Fatalf("POST /context/ack route=%q want room_context_ack", got)
	}
	if got := requestRouteName(http.MethodPost, "/v1/rooms/room_1/context"); got != "" {
		t.Fatalf("unexpected route for POST /context: %q", got)
	}
}
