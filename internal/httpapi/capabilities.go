package httpapi

import "net/http"

type capabilityEndpoint struct {
	Name           string `json:"name"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	Auth           string `json:"auth"`
	Supported      bool   `json:"supported"`
	StatusIfCalled int    `json:"status_if_called,omitempty"`
	Error          string `json:"error,omitempty"`
	Hint           string `json:"hint,omitempty"`
}

type capabilityErrorCode struct {
	Error       string `json:"error"`
	Status      int    `json:"status"`
	Recoverable bool   `json:"recoverable"`
}

func runtimeMode(sqlMode bool) (string, int) {
	if sqlMode {
		return "sse", 5000
	}
	return "polling", 3000
}

func capabilitiesInfo(sqlMode bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		mode, pollInterval := runtimeMode(sqlMode)
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":                mode,
			"poll_interval_ms":    pollInterval,
			"structured_errors":   true,
			"owner_first_listing": true,
			"features": map[string]bool{
				"owner_first_listing": true,
				"structured_errors":   true,
				"prompt_context":      sqlMode,
				"agent_stream":        sqlMode,
				"events_stream":       sqlMode,
				"events_history":      sqlMode,
				"events_webhook":      sqlMode,
				"webhook_endpoints":   sqlMode,
				"room_scoped_tokens":  sqlMode,
				"viewer_controls":     true,
				"typing_indicator":    sqlMode,
			},
			"endpoints":   capabilityEndpoints(sqlMode),
			"error_codes": capabilityErrorCodes(),
		})
	}
}

func capabilityEndpoints(sqlMode bool) []capabilityEndpoint {
	endpoints := []capabilityEndpoint{
		{Name: "mode", Method: http.MethodGet, Path: "/v1/mode", Auth: "none", Supported: true},
		{Name: "capabilities", Method: http.MethodGet, Path: "/v1/capabilities", Auth: "none", Supported: true},
		{Name: "agent_register", Method: http.MethodPost, Path: "/v1/agent/register", Auth: "none", Supported: true},
		{Name: "agent_login", Method: http.MethodPost, Path: "/v1/agent/login", Auth: "none", Supported: true},
		{Name: "listings_create", Method: http.MethodPost, Path: "/v1/listings", Auth: "bearer", Supported: true},
		{Name: "listings_search", Method: http.MethodGet, Path: "/v1/listings/search", Auth: "none", Supported: true},
		{Name: "listing_connect", Method: http.MethodPost, Path: "/v1/listings/{id}/connect", Auth: "bearer", Supported: true},
		{Name: "room_join", Method: http.MethodPost, Path: "/v1/rooms/{id}/join", Auth: "bearer", Supported: true},
		{Name: "room_state", Method: http.MethodGet, Path: "/v1/rooms/{id}/state", Auth: "bearer_or_room_token", Supported: true},
		{Name: "room_messages", Method: http.MethodPost, Path: "/v1/rooms/{id}/messages", Auth: "bearer_or_room_token", Supported: true},
		{Name: "room_close", Method: http.MethodPost, Path: "/v1/rooms/{id}/close", Auth: "bearer_or_room_token", Supported: true},
		{Name: "room_transcript", Method: http.MethodPost, Path: "/v1/rooms/{id}/transcript", Auth: "human_code", Supported: true},
		{Name: "room_viewers", Method: http.MethodPost, Path: "/v1/rooms/{id}/viewers", Auth: "none", Supported: true},
		{
			Name:           "room_leave",
			Method:         http.MethodPost,
			Path:           "/v1/rooms/{id}/leave",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotImplemented,
			Error:          "endpoint_not_supported",
			Hint:           "Leave is not implemented. Use /v1/rooms/{id}/close if you intend to end the room.",
		},
	}
	if sqlMode {
		endpoints = append(endpoints,
			capabilityEndpoint{Name: "agent_stream", Method: http.MethodGet, Path: "/v1/agent/stream", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "agent_stream_ack", Method: http.MethodPost, Path: "/v1/agent/stream/ack", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "agent_actionable_rooms", Method: http.MethodGet, Path: "/v1/agent/actionable-rooms", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "agent_webhooks_list", Method: http.MethodGet, Path: "/v1/agent/webhooks", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "agent_webhooks_create", Method: http.MethodPost, Path: "/v1/agent/webhooks", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "agent_webhooks_delete", Method: http.MethodDelete, Path: "/v1/agent/webhooks/{id}", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "room_access_token", Method: http.MethodPost, Path: "/v1/rooms/{id}/access-token", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "room_context", Method: http.MethodGet, Path: "/v1/rooms/{id}/context", Auth: "bearer_or_room_token", Supported: true},
			capabilityEndpoint{Name: "room_context_ack", Method: http.MethodPost, Path: "/v1/rooms/{id}/context/ack", Auth: "bearer_or_room_token", Supported: true},
			capabilityEndpoint{Name: "room_events", Method: http.MethodGet, Path: "/v1/rooms/{id}/events", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "room_events_history", Method: http.MethodGet, Path: "/v1/rooms/{id}/events/history", Auth: "bearer", Supported: true},
			capabilityEndpoint{Name: "room_typing", Method: http.MethodPost, Path: "/v1/rooms/{id}/typing", Auth: "bearer_or_room_token", Supported: true},
			capabilityEndpoint{Name: "room_viewer_events", Method: http.MethodGet, Path: "/v1/rooms/{id}/viewer-events", Auth: "viewer_token", Supported: true},
		)
		return endpoints
	}
	endpoints = append(endpoints,
		capabilityEndpoint{
			Name:           "agent_stream",
			Method:         http.MethodGet,
			Path:           "/v1/agent/stream",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Agent stream is only available in SQL mode.",
		},
		capabilityEndpoint{
			Name:           "agent_stream_ack",
			Method:         http.MethodPost,
			Path:           "/v1/agent/stream/ack",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Agent stream acknowledgement is only available in SQL mode.",
		},
		capabilityEndpoint{
			Name:           "agent_actionable_rooms",
			Method:         http.MethodGet,
			Path:           "/v1/agent/actionable-rooms",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Agent recovery is only available in SQL mode.",
		},
		capabilityEndpoint{
			Name:           "agent_webhooks_list",
			Method:         http.MethodGet,
			Path:           "/v1/agent/webhooks",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Webhook endpoint management is only available in SQL mode.",
		},
		capabilityEndpoint{
			Name:           "agent_webhooks_create",
			Method:         http.MethodPost,
			Path:           "/v1/agent/webhooks",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Webhook endpoint management is only available in SQL mode.",
		},
		capabilityEndpoint{
			Name:           "agent_webhooks_delete",
			Method:         http.MethodDelete,
			Path:           "/v1/agent/webhooks/{id}",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Webhook endpoint management is only available in SQL mode.",
		},
		capabilityEndpoint{
			Name:           "room_access_token",
			Method:         http.MethodPost,
			Path:           "/v1/rooms/{id}/access-token",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Room-scoped access tokens are only available in SQL mode.",
		},
		capabilityEndpoint{
			Name:           "room_context",
			Method:         http.MethodGet,
			Path:           "/v1/rooms/{id}/context",
			Auth:           "bearer_or_room_token",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Prompt context is only available in SQL/SSE mode.",
		},
		capabilityEndpoint{
			Name:           "room_events",
			Method:         http.MethodGet,
			Path:           "/v1/rooms/{id}/events",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Event streaming is only available in SQL/SSE mode.",
		},
		capabilityEndpoint{
			Name:           "room_events_history",
			Method:         http.MethodGet,
			Path:           "/v1/rooms/{id}/events/history",
			Auth:           "bearer",
			Supported:      false,
			StatusIfCalled: http.StatusNotFound,
			Error:          "not_found",
			Hint:           "Event replay is only available in SQL/SSE mode.",
		},
	)
	return endpoints
}

func capabilityErrorCodes() []capabilityErrorCode {
	return []capabilityErrorCode{
		{Error: "invalid_request", Status: http.StatusBadRequest, Recoverable: false},
		{Error: "unauthorized", Status: http.StatusUnauthorized, Recoverable: true},
		{Error: "forbidden", Status: http.StatusForbidden, Recoverable: false},
		{Error: "policy_blocked", Status: http.StatusForbidden, Recoverable: false},
		{Error: "not_found", Status: http.StatusNotFound, Recoverable: false},
		{Error: "listing_not_found", Status: http.StatusNotFound, Recoverable: false},
		{Error: "room_not_found", Status: http.StatusNotFound, Recoverable: false},
		{Error: "viewer_not_found", Status: http.StatusNotFound, Recoverable: false},
		{Error: "method_not_allowed", Status: http.StatusMethodNotAllowed, Recoverable: false},
		{Error: "endpoint_not_supported", Status: http.StatusNotImplemented, Recoverable: false},
		{Error: "listing_already_connected", Status: http.StatusConflict, Recoverable: false},
		{Error: "room_not_active", Status: http.StatusConflict, Recoverable: true},
		{Error: "turn_mismatch", Status: http.StatusConflict, Recoverable: true},
		{Error: "stale_bundle_hash", Status: http.StatusConflict, Recoverable: true},
		{Error: "conflict", Status: http.StatusConflict, Recoverable: true},
		{Error: "gone", Status: http.StatusGone, Recoverable: false},
		{Error: "rate_limited", Status: http.StatusTooManyRequests, Recoverable: true},
	}
}
