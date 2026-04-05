package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func capabilityEndpointSupported(t *testing.T, endpoints []any, name string) bool {
	t.Helper()
	for _, raw := range endpoints {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if got, _ := item["name"].(string); got == name {
			supported, _ := item["supported"].(bool)
			return supported
		}
	}
	t.Fatalf("capability endpoint %q not found in %v", name, endpoints)
	return false
}

func TestCapabilitiesEndpointInMemory(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	w := httptest.NewRecorder()

	capabilitiesInfo(false).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := body["mode"].(string); got != "polling" {
		t.Fatalf("mode=%v want=polling body=%v", body["mode"], body)
	}
	if got, _ := body["owner_first_listing"].(bool); !got {
		t.Fatalf("owner_first_listing=%v body=%v", body["owner_first_listing"], body)
	}
	features, ok := body["features"].(map[string]any)
	if !ok {
		t.Fatalf("features payload invalid: %v", body["features"])
	}
	if got, _ := features["events_webhook"].(bool); got {
		t.Fatalf("events_webhook=%v want=false in polling mode", got)
	}
	if got, _ := features["typing_indicator"].(bool); got {
		t.Fatalf("typing_indicator=%v want=false in polling mode", got)
	}
	endpoints, ok := body["endpoints"].([]any)
	if !ok {
		t.Fatalf("endpoints payload invalid: %v", body["endpoints"])
	}
	if capabilityEndpointSupported(t, endpoints, "agent_webhooks_create") {
		t.Fatal("agent_webhooks_create should be unsupported in polling mode")
	}
	if capabilityEndpointSupported(t, endpoints, "agent_stream") {
		t.Fatal("agent_stream should be unsupported in polling mode")
	}
	if capabilityEndpointSupported(t, endpoints, "agent_stream_ack") {
		t.Fatal("agent_stream_ack should be unsupported in polling mode")
	}
	if capabilityEndpointSupported(t, endpoints, "agent_actionable_rooms") {
		t.Fatal("agent_actionable_rooms should be unsupported in polling mode")
	}
	if capabilityEndpointSupported(t, endpoints, "room_access_token") {
		t.Fatal("room_access_token should be unsupported in polling mode")
	}
}

func TestCapabilitiesEndpointSQL(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	w := httptest.NewRecorder()

	capabilitiesInfo(true).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := body["mode"].(string); got != "sse" {
		t.Fatalf("mode=%v want=sse body=%v", body["mode"], body)
	}
	features, ok := body["features"].(map[string]any)
	if !ok {
		t.Fatalf("features payload invalid: %v", body["features"])
	}
	if got, _ := features["events_webhook"].(bool); !got {
		t.Fatalf("events_webhook=%v want=true in SQL mode", got)
	}
	endpoints, ok := body["endpoints"].([]any)
	if !ok {
		t.Fatalf("endpoints payload invalid: %v", body["endpoints"])
	}
	if !capabilityEndpointSupported(t, endpoints, "agent_webhooks_create") {
		t.Fatal("agent_webhooks_create should be supported in SQL mode")
	}
	if !capabilityEndpointSupported(t, endpoints, "agent_stream") {
		t.Fatal("agent_stream should be supported in SQL mode")
	}
	if !capabilityEndpointSupported(t, endpoints, "agent_stream_ack") {
		t.Fatal("agent_stream_ack should be supported in SQL mode")
	}
	if !capabilityEndpointSupported(t, endpoints, "agent_actionable_rooms") {
		t.Fatal("agent_actionable_rooms should be supported in SQL mode")
	}
	if !capabilityEndpointSupported(t, endpoints, "room_access_token") {
		t.Fatal("room_access_token should be supported in SQL mode")
	}
	if !capabilityEndpointSupported(t, endpoints, "room_context_ack") {
		t.Fatal("room_context_ack should be supported in SQL mode")
	}
	if !capabilityEndpointSupported(t, endpoints, "room_typing") {
		t.Fatal("room_typing should be supported in SQL mode")
	}
	if !capabilityEndpointSupported(t, endpoints, "room_viewer_events") {
		t.Fatal("room_viewer_events should be supported in SQL mode")
	}
	if got, _ := features["typing_indicator"].(bool); !got {
		t.Fatalf("typing_indicator=%v want=true in SQL mode", got)
	}
}
