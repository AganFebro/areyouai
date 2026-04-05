package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/febrian/areyouai/internal/domain"
	"github.com/febrian/areyouai/internal/repository"
	"github.com/febrian/areyouai/internal/security"
	"github.com/febrian/areyouai/internal/service/a2a"
)

const (
	maxActiveStreamsPerAgentRoom      = 5
	maxStreamConnectsPerMinuteRoomKey = 30
	maxStreamConnectsPerMinuteIP      = 120
)

type sqlHTTP struct {
	store                  repository.Store
	svc                    *a2a.Service
	hub                    *roomEventHub
	typingHub              *typingHub
	viewerHeartbeatTimeout time.Duration
	// Optional distributed stream coordination (implemented by Postgres store).
	streamLeaseStore repository.RoomEventStreamLeaseStore

	mu                sync.Mutex
	ipWindows         map[string][]time.Time
	streamCounts      map[string]int
	streamOpenWindows map[string][]time.Time
	streamIPWindows   map[string][]time.Time
	adminToken        string
}

func newSQLHTTP(store repository.Store, opts options) *sqlHTTP {
	hub := newRoomEventHub(64)
	typingHub := newTypingHub(64)
	viewerHeartbeatTimeout := opts.ViewerHeartbeatTimeout
	if viewerHeartbeatTimeout <= 0 {
		viewerHeartbeatTimeout = 45 * time.Second
	}
	var streamLeaseStore repository.RoomEventStreamLeaseStore
	if leaseStore, ok := store.(repository.RoomEventStreamLeaseStore); ok {
		streamLeaseStore = leaseStore
	}
	return &sqlHTTP{
		store: store,
		svc: a2a.New(store, a2a.Options{
			ViewerHeartbeatTimeout: opts.ViewerHeartbeatTimeout,
			ClosedRoomGraceDelay:   opts.ClosedRoomGraceDelay,
			MaxClosedRetention:     opts.MaxClosedRetention,
			RoomEventPublisher:     hub.Publish,
			WebhookSecretKey:       opts.WebhookSecretKey,
			WebhookSecretKeyset:    opts.WebhookSecretKeyset,
			RoomDEKKey:             opts.RoomDEKKey,
			RoomDEKKeyset:          opts.RoomDEKKeyset,
		}),
		hub:                    hub,
		typingHub:              typingHub,
		viewerHeartbeatTimeout: viewerHeartbeatTimeout,
		streamLeaseStore:       streamLeaseStore,
		ipWindows:              make(map[string][]time.Time),
		streamCounts:           make(map[string]int),
		streamOpenWindows:      make(map[string][]time.Time),
		streamIPWindows:        make(map[string][]time.Time),
		adminToken:             strings.TrimSpace(opts.AdminToken),
	}
}

func (s *sqlHTTP) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	out, err := s.svc.RegisterAgent(r.Context(), req.Name)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, registerResponse{
		AgentID: out.AgentID,
		APIKey:  out.APIKey,
	})
}

func (s *sqlHTTP) handleAgentLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	out, err := s.svc.Login(r.Context(), req.APIKey)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{SessionToken: out.SessionToken})
}

type agentWebhookEndpointRequest struct {
	URL     string `json:"url"`
	Secret  string `json:"secret"`
	KeyID   string `json:"key_id,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type agentStreamAckRequest struct {
	DeliveryID string `json:"delivery_id"`
}

func (s *sqlHTTP) handleAgentWebhooks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/agent/webhooks" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}

	switch r.Method {
	case http.MethodGet:
		items, err := s.svc.ListAgentWebhookEndpoints(r.Context(), agentID)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": sanitizeWebhookEndpoints(items),
		})
	case http.MethodPost:
		var req agentWebhookEndpointRequest
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request")
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		out, err := s.svc.CreateAgentWebhookEndpoint(r.Context(), agentID, req.URL, req.Secret, req.KeyID, enabled)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, sanitizeWebhookEndpoint(out.Endpoint))
	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *sqlHTTP) handleAgentWebhookByID(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "agent" || parts[2] != "webhooks" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodDelete {
		writeMethodNotAllowed(w, http.MethodDelete)
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	if err := s.svc.DeleteAgentWebhookEndpoint(r.Context(), agentID, parts[3]); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *sqlHTTP) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/agent/stream" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	cursor := parseAgentStreamCursor(r)
	resume, err := s.svc.ResolveAgentStreamResume(r.Context(), agentID, cursor)
	if err != nil {
		writeServiceErr(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	hello := map[string]any{
		"type":                          "stream.hello",
		"agent_id":                      agentID,
		"resume_status":                 resume.ResumeStatus,
		"last_acknowledged_delivery_id": resume.LastAcknowledgedDelivery,
		"server_time":                   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeAgentStreamControlEvent(w, flusher, "stream.hello", hello); err != nil {
		return
	}
	if resume.ResumeStatus == "replay_required" {
		_ = writeAgentStreamControlEvent(w, flusher, "stream.replay_required", map[string]any{
			"type":     "stream.replay_required",
			"agent_id": agentID,
			"hint":     "Call /v1/agent/actionable-rooms, clear the local cursor, and reconnect without Last-Event-ID.",
		})
		return
	}

	lastSentSeq := resume.AfterSeq
	if nextSeq, writeErr := s.writePendingAgentStreamBatch(r.Context(), w, flusher, agentID, lastSentSeq, 100); writeErr != nil {
		return
	} else {
		lastSentSeq = nextSeq
	}

	queryTicker := time.NewTicker(1 * time.Second)
	keepAliveTicker := time.NewTicker(20 * time.Second)
	reauthTicker := time.NewTicker(30 * time.Second)
	defer queryTicker.Stop()
	defer keepAliveTicker.Stop()
	defer reauthTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-queryTicker.C:
			nextSeq, writeErr := s.writePendingAgentStreamBatch(r.Context(), w, flusher, agentID, lastSentSeq, 100)
			if writeErr != nil {
				return
			}
			lastSentSeq = nextSeq
		case <-reauthTicker.C:
			if _, authErr := s.authAgentID(r.Context(), r); authErr != nil {
				_ = writeAgentStreamControlEvent(w, flusher, "auth.relogin_required", map[string]any{
					"type":     "auth.relogin_required",
					"agent_id": agentID,
				})
				return
			}
		case <-keepAliveTicker.C:
			if _, writeErr := io.WriteString(w, ": keepalive\n\n"); writeErr != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *sqlHTTP) handleAgentStreamAck(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/agent/stream/ack" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	var req agentStreamAckRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.svc.AckAgentStreamDelivery(r.Context(), agentID, req.DeliveryID); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"delivery_id": strings.TrimSpace(req.DeliveryID),
		"status":      "acked",
	})
}

func (s *sqlHTTP) handleAgentActionableRooms(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/agent/actionable-rooms" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out, err := s.svc.ActionableRooms(r.Context(), agentID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *sqlHTTP) handleListings(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/listings" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}

	var req createListingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	out, err := s.svc.CreateListing(r.Context(), agentID, req.Topic, req.Tags, req.MaxTurns, req.TTLSeconds)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":            out.Listing.ID,
		"agent_id":      out.Listing.AgentID,
		"topic":         out.Listing.Topic,
		"tags":          out.Listing.Tags,
		"max_turns":     out.Listing.MaxTurns,
		"ttl_seconds":   out.Listing.TTLSeconds,
		"created_at":    out.Listing.CreatedAt,
		"connected":     out.Listing.Connected,
		"room_id":       out.RoomID,
		"human_code":    out.HumanCode,
		"owner_joined":  out.OwnerJoined,
		"room_state":    string(out.RoomState),
		"next_actor_id": out.NextActorID,
	})
}

func (s *sqlHTTP) handleListingSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	results, err := s.svc.SearchListings(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		writeListingServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": results})
}

func (s *sqlHTTP) handleListingByID(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "listings" || parts[3] != "connect" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out, err := s.svc.ConnectListing(r.Context(), agentID, parts[2])
	if err != nil {
		writeListingServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"room_id":       out.RoomID,
		"human_code":    out.HumanCode,
		"agent_a_id":    out.AgentAID,
		"agent_b_id":    out.AgentBID,
		"room_state":    string(out.RoomState),
		"listing_id":    out.ListingID,
		"next_turn_a":   out.NextTurnA,
		"next_actor_id": out.NextActorID,
	})
}

func (s *sqlHTTP) handleRoomByID(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "rooms" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	roomID := parts[2]
	if len(parts) == 4 {
		switch parts[3] {
		case "join":
			s.handleRoomJoin(w, r, roomID)
		case "access-token":
			s.handleRoomAccessToken(w, r, roomID)
		case "messages":
			s.handleRoomMessage(w, r, roomID)
		case "state":
			s.handleRoomState(w, r, roomID)
		case "context":
			s.handleRoomContext(w, r, roomID)
		case "typing":
			s.handleRoomTyping(w, r, roomID)
		case "events":
			s.handleRoomEvents(w, r, roomID)
		case "viewer-events":
			s.handleRoomViewerEvents(w, r, roomID)
		case "leave":
			s.handleRoomLeave(w, r, roomID)
		case "close":
			s.handleRoomClose(w, r, roomID)
		case "transcript":
			s.handleTranscript(w, r, roomID)
		case "viewers":
			s.handleRoomViewers(w, r, roomID)
		default:
			writeError(w, http.StatusNotFound, "not found")
		}
		return
	}
	if len(parts) == 5 && parts[3] == "context" && parts[4] == "ack" {
		s.handleRoomContextAck(w, r, roomID)
		return
	}
	if len(parts) == 5 && parts[3] == "events" && parts[4] == "history" {
		s.handleRoomEventsHistory(w, r, roomID)
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

func (s *sqlHTTP) handleAdmin(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "admin" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if strings.TrimSpace(s.adminToken) == "" {
		writeError(w, http.StatusServiceUnavailable, "admin not configured")
		return
	}
	if strings.TrimSpace(r.Header.Get("X-Admin-Token")) != "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", errorOptions{
			Hint: "Use Authorization: Bearer <admin_token>. X-Admin-Token is not supported.",
		})
		return
	}
	if _, ok := r.URL.Query()["admin_token"]; ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", errorOptions{
			Hint: "Send admin credentials in Authorization header. Query-string admin_token is not supported.",
		})
		return
	}
	if !s.adminAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid admin token")
		return
	}

	switch parts[2] {
	case "overview":
		s.handleAdminOverview(w, r)
	case "rooms":
		s.handleAdminRooms(w, r)
	case "audit":
		s.handleAdminAudit(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func sanitizeWebhookEndpoints(items []repository.AgentWebhookEndpoint) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, sanitizeWebhookEndpoint(item))
	}
	return out
}

func sanitizeWebhookEndpoint(item repository.AgentWebhookEndpoint) map[string]any {
	return map[string]any{
		"id":         item.ID,
		"agent_id":   item.AgentID,
		"url":        item.URL,
		"key_id":     item.KeyID,
		"enabled":    item.Enabled,
		"created_at": item.CreatedAt,
		"updated_at": item.UpdatedAt,
	}
}

func (s *sqlHTTP) adminAuthorized(r *http.Request) bool {
	adminToken := strings.TrimSpace(s.adminToken)
	if adminToken == "" {
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		return false
	}
	return subtleConstantTimeEqual(token, adminToken)
}

func subtleConstantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	// Keep comparison timing resistant for token checks.
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (s *sqlHTTP) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	out, err := s.svcAdminOverview(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *sqlHTTP) handleAdminRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.svcAdminRooms(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rooms})
}

func (s *sqlHTTP) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	events, err := s.svcAdminAudit(r.Context(), 300)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": events})
}

func (s *sqlHTTP) svcAdminOverview(ctx context.Context) (map[string]any, error) {
	out, err := s.svc.AdminOverview(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"agents_total":    out.Overview.AgentsTotal,
		"sessions_active": out.Overview.SessionsActive,
		"rooms_open":      out.Overview.RoomsOpen,
		"rooms_active":    out.Overview.RoomsActive,
		"rooms_closed":    out.Overview.RoomsClosed,
		"rooms_purged":    out.Overview.RoomsPurged,
		"messages_total":  out.Overview.MessagesTotal,
		"purge": map[string]any{
			"scanned":                      out.Purge.Scanned,
			"closed_rooms":                 out.Purge.ClosedRooms,
			"ready_for_purge":              out.Purge.ReadyForPurge,
			"viewer_blocked":               out.Purge.ViewerBlocked,
			"over_retention":               out.Purge.OverRetention,
			"oldest_closed_age_seconds":    out.Purge.OldestClosedAgeSeconds,
			"oldest_ready_age_seconds":     out.Purge.OldestReadyAgeSeconds,
			"closed_room_grace_seconds":    out.Purge.ClosedRoomGraceSeconds,
			"max_closed_retention_seconds": out.Purge.MaxClosedRetentionSeconds,
		},
		"generated_at_utc": time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func (s *sqlHTTP) svcAdminRooms(ctx context.Context, limit int) ([]repository.AdminRoom, error) {
	return s.svc.AdminRooms(ctx, limit)
}

func (s *sqlHTTP) svcAdminAudit(ctx context.Context, limit int) ([]repository.AuditEvent, error) {
	return s.svc.AdminAudit(ctx, limit)
}

func (s *sqlHTTP) handleRoomContext(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authRoomAccess(r.Context(), r, roomID, "room:context")
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out, err := s.svc.GetPromptBundle(r.Context(), agentID, roomID)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":              roomID,
		"bundle_hash":          out.BundleHash,
		"system_core_hash":     out.SystemCoreHash,
		"global_rules_hash":    out.GlobalRulesHash,
		"agent_rules_hash":     out.AgentRulesHash,
		"identity_hash":        out.IdentityHash,
		"soul_hash":            out.SoulHash,
		"user_hash":            out.UserHash,
		"turn_index":           out.NextTurn,
		"next_turn":            out.NextTurn,
		"next_actor_id":        out.NextActorID,
		"context_ack_required": true,
		"context_ack_path":     "/v1/rooms/{id}/context/ack",
		"mode":                 "sse",
		"poll_interval_ms":     5000,
		"ordered_stack":        out.OrderedStack,
		"prompt_bundle_text":   out.Prompt,
	})
}

func (s *sqlHTTP) handleRoomContextAck(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authRoomAccess(r.Context(), r, roomID, "room:context")
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	var req roomContextAckRequest
	if err := decodeJSON(w, r, &req); err != nil || req.TurnIndex == nil || *req.TurnIndex < 0 {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.svc.RecordRoomContextFetch(r.Context(), agentID, roomID, *req.TurnIndex); err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":    roomID,
		"agent_id":   agentID,
		"turn_index": *req.TurnIndex,
	})
}

func (s *sqlHTTP) handleRoomJoin(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	state, joined, err := s.svc.JoinRoom(r.Context(), agentID, roomID)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	bundle, err := s.svc.GetPromptBundle(r.Context(), agentID, roomID)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":           roomID,
		"state":             state,
		"joined":            joined,
		"initial_bundle":    bundle.BundleHash,
		"system_core_hash":  bundle.SystemCoreHash,
		"global_rules_hash": bundle.GlobalRulesHash,
		"agent_rules_hash":  bundle.AgentRulesHash,
		"identity_hash":     bundle.IdentityHash,
		"soul_hash":         bundle.SoulHash,
		"user_hash":         bundle.UserHash,
		"ordered_stack":     bundle.OrderedStack,
	})
}

func (s *sqlHTTP) handleRoomAccessToken(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out, err := s.svc.CreateRoomAccessToken(r.Context(), agentID, roomID)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"room_id":    out.RoomID,
		"agent_id":   out.AgentID,
		"token":      out.Token,
		"scope":      out.Scope,
		"expires_at": out.ExpiresAt,
	})
}

func (s *sqlHTTP) handleRoomMessage(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.allowIPMessage(r.RemoteAddr, time.Now().UTC()) {
		s.svc.AppendSecurityAudit(r.Context(), roomID, "ip_rate_limited", map[string]any{
			"room_id": roomID,
			"ip":      remoteIP(r.RemoteAddr),
		}, 0)
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	agentID, err := s.authRoomAccess(r.Context(), r, roomID, "room:message")
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	var req messageRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	out, err := s.svc.SendMessage(r.Context(), agentID, roomID, req.ExpectedTurn, req.Ciphertext, req.BundleHash)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	if s.typingHub != nil {
		s.typingHub.Stop(roomID, agentID, time.Now().UTC())
		if out.RoomState != domain.RoomStateActive {
			s.typingHub.ClearRoom(roomID, time.Now().UTC())
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"message_id":  out.Message.ID,
		"turn":        out.Message.Turn,
		"next_turn":   out.NextTurn,
		"room_state":  out.RoomState,
		"bundle_hash": out.BundleHash,
	})
}

func (s *sqlHTTP) handleRoomState(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authRoomAccess(r.Context(), r, roomID, "room:state")
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out, err := s.svc.GetRoomState(r.Context(), agentID, roomID)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":             out.Room.ID,
		"agent_a_id":     out.Room.AgentAID,
		"agent_b_id":     out.Room.AgentBID,
		"state":          out.Room.State,
		"turn_index":     out.Room.TurnIndex,
		"next_turn":      out.NextTurn,
		"next_actor_id":  out.NextActorID,
		"max_turns":      out.Room.MaxTurns,
		"ttl_at":         out.Room.TTLAt,
		"created_at":     out.Room.CreatedAt,
		"closed_at":      out.Room.ClosedAt,
		"purged_at":      out.Room.PurgedAt,
		"active_viewers": out.ActiveViewers,
	})
}

func (s *sqlHTTP) handleRoomClose(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authRoomAccess(r.Context(), r, roomID, "room:close")
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	rm, err := s.svc.CloseRoom(r.Context(), agentID, roomID)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	if s.typingHub != nil {
		s.typingHub.ClearRoom(roomID, time.Now().UTC())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id": rm.ID,
		"state":   rm.State,
	})
}

func (s *sqlHTTP) handleRoomLeave(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	writeEndpointNotSupported(w, "/v1/rooms/{id}/leave", "Leave is not implemented. Use /v1/rooms/{id}/close to end a room, or stop sending and wait for room closure.")
}

func (s *sqlHTTP) handleTranscript(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := r.URL.Query()["human_code"]; ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", errorOptions{
			Hint: "Send human_code in the JSON request body. Query-string human_code is not supported.",
		})
		return
	}
	var req transcriptRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	out, err := s.svc.Transcript(r.Context(), roomID, strings.TrimSpace(req.HumanCode))
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	messages := make([]map[string]any, 0, len(out.Messages))
	for _, msg := range out.Messages {
		readByOpponent := false
		opponentID := ""
		switch msg.SenderID {
		case out.Room.AgentAID:
			opponentID = out.Room.AgentBID
		case out.Room.AgentBID:
			opponentID = out.Room.AgentAID
		}
		if opponentID != "" {
			if fetchTurn, ok := out.LastContextFetchByAgent[opponentID]; ok && msg.Turn < fetchTurn {
				readByOpponent = true
			}
		}
		messages = append(messages, map[string]any{
			"id":               msg.ID,
			"sender_id":        msg.SenderID,
			"sender_name":      msg.SenderName,
			"turn":             msg.Turn,
			"ciphertext":       msg.Ciphertext,
			"created_at":       msg.CreatedAt,
			"read_by_opponent": readByOpponent,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":                          roomID,
		"room_topic":                       strings.TrimSpace(out.Room.Topic),
		"agent_a_id":                       out.Room.AgentAID,
		"agent_b_id":                       out.Room.AgentBID,
		"turn_index":                       out.Room.TurnIndex,
		"next_actor_id":                    nextActorIDForTranscript(out.Room),
		"state":                            out.Room.State,
		"messages":                         messages,
		"closed_at":                        out.Room.ClosedAt,
		"purged_at":                        out.Room.PurgedAt,
		"last_context_fetch_turn_by_agent": out.LastContextFetchByAgent,
	})
}

func (s *sqlHTTP) handleRoomViewers(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req viewerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch strings.TrimSpace(req.Op) {
	case "join":
		out, err := s.svc.ViewerJoin(r.Context(), roomID, req.HumanCode)
		if err != nil {
			writeRoomServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"viewer_token":   out.ViewerToken,
			"active_viewers": out.ActiveViewers,
		})
	case "heartbeat":
		out, err := s.svc.ViewerHeartbeat(r.Context(), roomID, req.ViewerToken)
		if err != nil {
			writeRoomOrViewerServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"active_viewers": out.ActiveViewers})
	case "leave":
		out, err := s.svc.ViewerLeave(r.Context(), roomID, req.ViewerToken)
		if err != nil {
			writeRoomOrViewerServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"active_viewers": out.ActiveViewers})
	default:
		writeError(w, http.StatusBadRequest, "unsupported op")
	}
}

func (s *sqlHTTP) handleRoomEvents(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	sinceID, limit, err := parseEventStreamQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	releaseStreamSlot, deniedReason, err := s.acquireStreamSlot(r.Context(), agentID, roomID, r.RemoteAddr, time.Now().UTC())
	if err != nil {
		subscriberCount := s.hub.SubscriberCount(roomID)
		meta := map[string]any{
			"room_id":          roomID,
			"agent_id":         agentID,
			"event_id":         sinceID,
			"subscriber_count": subscriberCount,
			"drop_reason":      deniedReason,
			"remote_ip":        remoteIP(r.RemoteAddr),
		}
		s.svc.AppendSecurityAudit(r.Context(), roomID, "stream_dropped", meta, 0)
		log.Printf(
			"stream_dropped room_id=%s agent_id=%s event_id=%d subscriber_count=%d drop_reason=%s remote_ip=%s",
			roomID,
			agentID,
			sinceID,
			subscriberCount,
			deniedReason,
			remoteIP(r.RemoteAddr),
		)
		writeServiceErr(w, err)
		return
	}
	defer releaseStreamSlot()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	closeReason := "client_disconnected"

	sub := s.hub.Subscribe(roomID)
	defer sub.Close()

	lastEventID := sinceID
	for {
		replay, err := s.svc.ListRoomEventHistory(r.Context(), agentID, roomID, lastEventID, limit)
		if err != nil {
			writeRoomServiceErr(w, err)
			return
		}
		if len(replay.Items) == 0 {
			break
		}
		for _, item := range replay.Items {
			if err := writeSSEEvent(w, flusher, item); err != nil {
				closeReason = "write_failed"
				return
			}
			lastEventID = item.ID
		}
		if len(replay.Items) < limit {
			break
		}
	}

	sinceID = lastEventID
	subscriberCount := s.hub.SubscriberCount(roomID)
	openMeta := map[string]any{
		"room_id":          roomID,
		"agent_id":         agentID,
		"event_id":         sinceID,
		"since_id":         sinceID,
		"subscriber_count": subscriberCount,
		"remote_ip":        remoteIP(r.RemoteAddr),
	}
	s.svc.AppendSecurityAudit(r.Context(), roomID, "stream_opened", openMeta, 0)
	log.Printf(
		"stream_opened room_id=%s agent_id=%s event_id=%d subscriber_count=%d remote_ip=%s",
		roomID,
		agentID,
		sinceID,
		subscriberCount,
		remoteIP(r.RemoteAddr),
	)

	defer func() {
		eventName := "stream_closed"
		if closeReason == "dropped_slow_subscriber" {
			eventName = "stream_dropped"
		}
		meta := map[string]any{
			"room_id":          roomID,
			"agent_id":         agentID,
			"event_id":         lastEventID,
			"subscriber_count": s.hub.SubscriberCount(roomID),
			"drop_reason":      closeReason,
			"remote_ip":        remoteIP(r.RemoteAddr),
		}
		s.svc.AppendSecurityAudit(r.Context(), roomID, eventName, meta, 0)
		log.Printf(
			"%s room_id=%s agent_id=%s event_id=%d subscriber_count=%d drop_reason=%s remote_ip=%s",
			eventName,
			roomID,
			agentID,
			lastEventID,
			s.hub.SubscriberCount(roomID),
			closeReason,
			remoteIP(r.RemoteAddr),
		)
	}()

	keepAliveTicker := time.NewTicker(20 * time.Second)
	reauthTicker := time.NewTicker(30 * time.Second)
	defer keepAliveTicker.Stop()
	defer reauthTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			closeReason = "client_disconnected"
			return
		case item, ok := <-sub.Events():
			if !ok {
				// Dropped from hub (slow consumer), client should reconnect.
				if sub.Dropped() {
					closeReason = "dropped_slow_subscriber"
				} else {
					closeReason = "subscription_closed"
				}
				return
			}
			if item.ID <= sinceID || item.RoomID != roomID {
				continue
			}
			if err := writeSSEEvent(w, flusher, item); err != nil {
				closeReason = "write_failed"
				return
			}
			sinceID = item.ID
			lastEventID = item.ID
		case <-reauthTicker.C:
			if _, authErr := s.authAgentID(r.Context(), r); authErr != nil {
				closeReason = "auth_revalidation_failed"
				return
			}
		case <-keepAliveTicker.C:
			if _, writeErr := io.WriteString(w, ": keepalive\n\n"); writeErr != nil {
				closeReason = "keepalive_write_failed"
				return
			}
			flusher.Flush()
		}
	}
}

func (s *sqlHTTP) handleRoomEventsHistory(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}

	sinceID, limit, err := parseEventHistoryQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	out, err := s.svc.ListRoomEventHistory(r.Context(), agentID, roomID, sinceID, limit)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}

	items := make([]map[string]any, 0, len(out.Items))
	for _, item := range out.Items {
		items = append(items, roomEventPayload(item))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":    roomID,
		"items":      items,
		"next_since": out.NextSince,
	})
}

type roomTypingRequest struct {
	State string `json:"state"`
	TTLMs *int   `json:"ttl_ms,omitempty"`
}

func (s *sqlHTTP) handleRoomTyping(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authRoomAccess(r.Context(), r, roomID, "room:typing")
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	var req roomTypingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	state := strings.TrimSpace(req.State)
	if state != "start" && state != "stop" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", errorOptions{
			Hint: "state must be start or stop.",
		})
		return
	}

	room, err := s.svc.GetRoomState(r.Context(), agentID, roomID)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}

	now := time.Now().UTC()
	if state == "start" {
		if room.Room.State != domain.RoomStateActive {
			writeAPIError(w, http.StatusConflict, "room_not_active", errorOptions{
				Recoverable: true,
				Hint:        "Wait until the room becomes ACTIVE before sending typing updates.",
			})
			return
		}
		if room.NextActorID != agentID {
			writeAPIError(w, http.StatusConflict, "turn_mismatch", errorOptions{
				Recoverable: true,
				Hint:        "Only the current speaker may emit typing.start.",
			})
			return
		}
		ttl := defaultTypingTTL
		if req.TTLMs != nil {
			if *req.TTLMs <= 0 {
				writeAPIError(w, http.StatusBadRequest, "invalid_request", errorOptions{
					Hint: "ttl_ms must be greater than 0 when provided.",
				})
				return
			}
			ttl = time.Duration(*req.TTLMs) * time.Millisecond
			if ttl < minTypingTTL {
				ttl = minTypingTTL
			}
			if ttl > maxTypingTTL {
				ttl = maxTypingTTL
			}
		}
		event := roomTypingEvent{}
		if s.typingHub != nil {
			event = s.typingHub.Start(roomID, agentID, now, ttl)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"room_id":    roomID,
			"actor_id":   agentID,
			"state":      "start",
			"ttl_ms":     int(ttl / time.Millisecond),
			"expires_at": event.ExpiresAt,
		})
		return
	}

	var event roomTypingEvent
	cleared := false
	if s.typingHub != nil {
		event, cleared = s.typingHub.Stop(roomID, agentID, now)
	}
	_ = event
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":  roomID,
		"actor_id": agentID,
		"state":    "stop",
		"cleared":  cleared,
	})
}

func (s *sqlHTTP) handleRoomViewerEvents(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	token := bearerTokenFromRequest(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	viewer, err := s.store.GetViewer(r.Context(), token)
	if err != nil {
		writeRoomOrViewerServiceErr(w, err)
		return
	}
	if viewer.RoomID != roomID {
		writeAPIError(w, http.StatusNotFound, "viewer_not_found", errorOptions{})
		return
	}
	if viewer.LeftAt != nil {
		writeAPIError(w, http.StatusGone, "gone", errorOptions{})
		return
	}
	if viewerHeartbeatExpired(viewer.LastHeartbeatAt, time.Now().UTC(), s.viewerHeartbeatTimeout) {
		writeAPIError(w, http.StatusNotFound, "viewer_not_found", errorOptions{})
		return
	}
	room, err := s.svc.RoomSnapshot(r.Context(), roomID)
	if err != nil {
		writeRoomServiceErr(w, err)
		return
	}
	if room.State != domain.RoomStateActive {
		writeAPIError(w, http.StatusGone, "gone", errorOptions{})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	if s.typingHub != nil {
		sub, snapshot := s.typingHub.SubscribeWithSnapshot(roomID, time.Now().UTC())
		defer sub.Close()
		for _, ev := range snapshot {
			if err := writeTypingSSEEvent(w, flusher, ev); err != nil {
				return
			}
		}

		keepAliveTicker := time.NewTicker(20 * time.Second)
		reauthTicker := time.NewTicker(viewerStreamReauthInterval(s.viewerHeartbeatTimeout))
		defer keepAliveTicker.Stop()
		defer reauthTicker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-sub.Events():
				if !ok {
					return
				}
				if ev.RoomID != roomID {
					continue
				}
				if err := writeTypingSSEEvent(w, flusher, ev); err != nil {
					return
				}
			case <-reauthTicker.C:
				now := time.Now().UTC()
				currentViewer, authErr := s.store.GetViewer(r.Context(), token)
				if authErr != nil || currentViewer.RoomID != roomID || currentViewer.LeftAt != nil || viewerHeartbeatExpired(currentViewer.LastHeartbeatAt, now, s.viewerHeartbeatTimeout) {
					return
				}
				currentRoom, roomErr := s.svc.RoomSnapshot(r.Context(), roomID)
				if roomErr != nil || currentRoom.State != domain.RoomStateActive {
					return
				}
			case <-keepAliveTicker.C:
				if _, writeErr := io.WriteString(w, ": keepalive\n\n"); writeErr != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

func parseEventStreamQuery(r *http.Request) (int64, int, error) {
	sinceID, limit, err := parseEventHistoryQuery(r)
	if err != nil {
		return 0, 0, err
	}
	if strings.TrimSpace(r.URL.Query().Get("since")) != "" {
		return sinceID, limit, nil
	}
	lastEventID := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if lastEventID == "" {
		return sinceID, limit, nil
	}
	v, err := strconv.ParseInt(lastEventID, 10, 64)
	if err != nil || v < 0 {
		return 0, 0, errors.New("invalid last-event-id")
	}
	return v, limit, nil
}

func parseAgentStreamCursor(r *http.Request) string {
	if raw := strings.TrimSpace(r.URL.Query().Get("last_delivery_id")); raw != "" {
		return raw
	}
	return strings.TrimSpace(r.Header.Get("Last-Event-ID"))
}

func parseEventHistoryQuery(r *http.Request) (int64, int, error) {
	since := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			return 0, 0, errors.New("invalid since")
		}
		since = v
	}

	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			return 0, 0, errors.New("invalid limit")
		}
		limit = v
	}
	return since, limit, nil
}

func nextActorIDForTranscript(rm repository.Room) string {
	if rm.State != domain.RoomStateOpen && rm.State != domain.RoomStateActive {
		return ""
	}
	if rm.TurnIndex%2 == 0 {
		return rm.AgentAID
	}
	return rm.AgentBID
}

func roomEventPayload(item repository.RoomEvent) map[string]any {
	payload := map[string]any{
		"event_id":   item.ID,
		"type":       item.EventType,
		"room_id":    item.RoomID,
		"created_at": item.CreatedAt,
	}
	if item.MessageID != nil {
		payload["message_id"] = *item.MessageID
	}
	if item.Turn != nil {
		payload["turn"] = *item.Turn
	}
	if item.SenderID != nil {
		payload["sender_id"] = *item.SenderID
	}
	if item.Ciphertext != nil {
		payload["ciphertext"] = *item.Ciphertext
	}
	return payload
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, item repository.RoomEvent) error {
	payload, err := json.Marshal(roomEventPayload(item))
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("id: %d\n", item.ID)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("event: %s\n", item.EventType)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeTypingSSEEvent(w http.ResponseWriter, flusher http.Flusher, item roomTypingEvent) error {
	payload, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("event: %s\n", strings.TrimSpace(item.Type))); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func viewerHeartbeatExpired(lastHeartbeatAt, now time.Time, timeout time.Duration) bool {
	if lastHeartbeatAt.IsZero() {
		return true
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return now.Sub(lastHeartbeatAt) > timeout
}

func viewerStreamReauthInterval(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	interval := timeout / 2
	if interval < 500*time.Millisecond {
		return 500 * time.Millisecond
	}
	if interval > 10*time.Second {
		return 10 * time.Second
	}
	return interval
}

func writeAgentStreamControlEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("event: %s\n", strings.TrimSpace(eventType))); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeAgentStreamDeliveryEvent(w http.ResponseWriter, flusher http.Flusher, item repository.AgentStreamDelivery) error {
	payload := agentStreamDeliveryPayload(item)
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("id: %s\n", item.DeliveryID)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("event: %s\n", item.Type)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func agentStreamDeliveryPayload(item repository.AgentStreamDelivery) map[string]any {
	out := map[string]any{}
	if len(item.Payload) > 0 {
		_ = json.Unmarshal(item.Payload, &out)
	}
	out["delivery_id"] = item.DeliveryID
	out["type"] = item.Type
	out["agent_id"] = item.AgentID
	out["room_id"] = item.RoomID
	out["created_at"] = item.CreatedAt
	out["expires_at"] = item.ExpiresAt
	return out
}

func (s *sqlHTTP) writePendingAgentStreamBatch(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, agentID string, afterSeq int64, limit int) (int64, error) {
	items, err := s.svc.ListPendingAgentStreamDeliveries(ctx, agentID, afterSeq, limit)
	if err != nil {
		return afterSeq, err
	}
	lastSeq := afterSeq
	for _, item := range items {
		if err := writeAgentStreamDeliveryEvent(w, flusher, item); err != nil {
			return lastSeq, err
		}
		lastSeq = item.Seq
	}
	return lastSeq, nil
}

func (s *sqlHTTP) authAgentID(ctx context.Context, r *http.Request) (string, error) {
	token := bearerTokenFromRequest(r)
	if token == "" {
		return "", a2a.ErrUnauthorized
	}
	return s.svc.AuthAgentID(ctx, token)
}

func (s *sqlHTTP) authRoomAccess(ctx context.Context, r *http.Request, roomID, action string) (string, error) {
	token := bearerTokenFromRequest(r)
	if token == "" {
		return "", a2a.ErrUnauthorized
	}
	return s.svc.AuthRoomAccess(ctx, token, roomID, action)
}

func bearerTokenFromRequest(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, a2a.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "invalid request")
	case errors.Is(err, a2a.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
	case errors.Is(err, a2a.ErrPolicyBlocked):
		writeError(w, http.StatusForbidden, "policy blocked")
	case errors.Is(err, a2a.ErrPayloadTooLarge):
		writeAPIError(w, http.StatusRequestEntityTooLarge, "payload_too_large", errorOptions{
			Recoverable: true,
			Hint:        fmt.Sprintf("Keep the message at or below %d characters.", security.MaxPersistMessageChars),
			MaxChars:    security.MaxPersistMessageChars,
		})
	case errors.Is(err, a2a.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, a2a.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, a2a.ErrTurnMismatch):
		writeError(w, http.StatusConflict, "turn_mismatch")
	case errors.Is(err, a2a.ErrStaleBundleHash):
		writeError(w, http.StatusConflict, "stale_bundle_hash")
	case errors.Is(err, a2a.ErrRoomNotActive):
		writeAPIError(w, http.StatusConflict, "room_not_active", errorOptions{
			Recoverable: true,
			Hint:        "Wait until the room becomes ACTIVE before sending messages.",
		})
	case errors.Is(err, a2a.ErrConflict):
		writeError(w, http.StatusConflict, "conflict")
	case errors.Is(err, a2a.ErrGone):
		writeError(w, http.StatusGone, "gone")
	case errors.Is(err, a2a.ErrRateLimit):
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeRoomServiceErr(w http.ResponseWriter, err error) {
	if errors.Is(err, a2a.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "room_not_found", errorOptions{})
		return
	}
	writeServiceErr(w, err)
}

func writeListingServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, a2a.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "listing_not_found", errorOptions{})
	case errors.Is(err, a2a.ErrConflict):
		writeAPIError(w, http.StatusConflict, "listing_already_connected", errorOptions{})
	default:
		writeServiceErr(w, err)
	}
}

func writeRoomOrViewerServiceErr(w http.ResponseWriter, err error) {
	if errors.Is(err, a2a.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "viewer_not_found", errorOptions{})
		return
	}
	writeRoomServiceErr(w, err)
}

func (s *sqlHTTP) acquireStreamSlot(ctx context.Context, agentID, roomID, remoteAddr string, now time.Time) (func(), string, error) {
	if s.streamLeaseStore != nil {
		return s.acquireDistributedStreamSlot(ctx, agentID, roomID, remoteAddr, now)
	}

	key := streamKey(agentID, roomID)
	ip := remoteIP(remoteAddr)
	if ip == "" {
		ip = "unknown"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.streamCounts == nil {
		s.streamCounts = make(map[string]int)
	}
	if s.streamOpenWindows == nil {
		s.streamOpenWindows = make(map[string][]time.Time)
	}
	if s.streamIPWindows == nil {
		s.streamIPWindows = make(map[string][]time.Time)
	}

	windowStart := now.Add(-1 * time.Minute)
	perRoomAgent := pruneTimeWindow(s.streamOpenWindows[key], windowStart)
	perIP := pruneTimeWindow(s.streamIPWindows[ip], windowStart)

	if s.streamCounts[key] >= maxActiveStreamsPerAgentRoom {
		return nil, "max_active_streams_per_agent_room", a2a.ErrRateLimit
	}
	if len(perRoomAgent) >= maxStreamConnectsPerMinuteRoomKey {
		s.streamOpenWindows[key] = perRoomAgent
		return nil, "reconnect_rate_limited_room_agent", a2a.ErrRateLimit
	}
	if len(perIP) >= maxStreamConnectsPerMinuteIP {
		s.streamIPWindows[ip] = perIP
		return nil, "reconnect_rate_limited_ip", a2a.ErrRateLimit
	}

	s.streamCounts[key]++
	s.streamOpenWindows[key] = append(perRoomAgent, now)
	s.streamIPWindows[ip] = append(perIP, now)

	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			curr := s.streamCounts[key]
			switch {
			case curr <= 1:
				delete(s.streamCounts, key)
			default:
				s.streamCounts[key] = curr - 1
			}
		})
	}
	return release, "", nil
}

func (s *sqlHTTP) acquireDistributedStreamSlot(ctx context.Context, agentID, roomID, remoteAddr string, now time.Time) (func(), string, error) {
	if s.streamLeaseStore == nil {
		return nil, "stream_coordination_unavailable", errors.New("stream lease store unavailable")
	}

	ip := remoteIP(remoteAddr)
	if ip == "" {
		ip = "unknown"
	}
	leaseID := newID("sls")
	out, err := s.streamLeaseStore.AcquireRoomEventStreamLease(ctx, repository.AcquireRoomEventStreamLeaseInput{
		LeaseID:                 leaseID,
		RoomID:                  strings.TrimSpace(roomID),
		AgentID:                 strings.TrimSpace(agentID),
		RemoteIP:                ip,
		Now:                     now,
		LeaseExpiresAt:          now.Add(90 * time.Second),
		MaxActivePerRoomAgent:   maxActiveStreamsPerAgentRoom,
		MaxConnectsPerMinuteKey: maxStreamConnectsPerMinuteRoomKey,
		MaxConnectsPerMinuteIP:  maxStreamConnectsPerMinuteIP,
	})
	if err != nil {
		return nil, "stream_coordination_failed", err
	}
	if !out.Acquired {
		reason := strings.TrimSpace(out.DeniedReason)
		if reason == "" {
			reason = "stream_rate_limited"
		}
		return nil, reason, a2a.ErrRateLimit
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			if err := s.streamLeaseStore.ReleaseRoomEventStreamLease(context.Background(), leaseID); err != nil {
				log.Printf("stream_lease_release_failed lease_id=%s room_id=%s agent_id=%s err=%v", leaseID, roomID, agentID, err)
			}
		})
	}
	return release, "", nil
}

func streamKey(agentID, roomID string) string {
	return strings.TrimSpace(roomID) + "|" + strings.TrimSpace(agentID)
}

func pruneTimeWindow(times []time.Time, keepAfter time.Time) []time.Time {
	if len(times) == 0 {
		return nil
	}
	kept := times[:0]
	for _, at := range times {
		if at.After(keepAfter) {
			kept = append(kept, at)
		}
	}
	return kept
}

func (s *sqlHTTP) allowIPMessage(addr string, now time.Time) bool {
	ip := remoteIP(addr)
	if ip == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	const maxPerMinuteIP = 120
	windowStart := now.Add(-1 * time.Minute)
	timestamps := s.ipWindows[ip]
	kept := timestamps[:0]
	for _, t := range timestamps {
		if t.After(windowStart) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxPerMinuteIP {
		s.ipWindows[ip] = kept
		return false
	}
	kept = append(kept, now)
	s.ipWindows[ip] = kept
	return true
}

func remoteIP(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.TrimSpace(addr)
	}
	return strings.TrimSpace(host)
}
