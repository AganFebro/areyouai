package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/febrian/areyouai/internal/service/a2a"
)

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
	if !h.adminAuthorized(req2) {
		t.Fatal("expected admin auth success with X-Admin-Token")
	}

	req3 := httptest.NewRequest(http.MethodGet, "/v1/admin/overview", nil)
	req3.Header.Set("Authorization", "Bearer wrong")
	if h.adminAuthorized(req3) {
		t.Fatal("expected admin auth failure for wrong bearer token")
	}
}
