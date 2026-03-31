package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/febrian/areyouai/internal/repository"
	"github.com/febrian/areyouai/internal/service/a2a"
)

type sqlHTTP struct {
	svc *a2a.Service

	mu         sync.Mutex
	ipWindows  map[string][]time.Time
	adminToken string
}

func newSQLHTTP(store repository.Store, opts options) *sqlHTTP {
	return &sqlHTTP{
		svc: a2a.New(store, a2a.Options{
			ViewerHeartbeatTimeout: opts.ViewerHeartbeatTimeout,
			ClosedRoomGraceDelay:   opts.ClosedRoomGraceDelay,
			MaxClosedRetention:     opts.MaxClosedRetention,
		}),
		ipWindows:  make(map[string][]time.Time),
		adminToken: strings.TrimSpace(opts.AdminToken),
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
	item, err := s.svc.CreateListing(r.Context(), agentID, req.Topic, req.Tags, req.MaxTurns, req.TTLSeconds)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          item.ID,
		"agent_id":    item.AgentID,
		"topic":       item.Topic,
		"tags":        item.Tags,
		"max_turns":   item.MaxTurns,
		"ttl_seconds": item.TTLSeconds,
		"created_at":  item.CreatedAt,
		"connected":   item.Connected,
	})
}

func (s *sqlHTTP) handleListingSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	results, err := s.svc.SearchListings(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		writeServiceErr(w, err)
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
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"room_id":     out.RoomID,
		"human_code":  out.HumanCode,
		"agent_a_id":  out.AgentAID,
		"agent_b_id":  out.AgentBID,
		"room_state":  string(out.RoomState),
		"listing_id":  out.ListingID,
		"next_turn_a": out.NextTurnA,
	})
}

func (s *sqlHTTP) handleRoomByID(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "rooms" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	roomID, action := parts[2], parts[3]
	switch action {
	case "join":
		s.handleRoomJoin(w, r, roomID)
	case "messages":
		s.handleRoomMessage(w, r, roomID)
	case "state":
		s.handleRoomState(w, r, roomID)
	case "context":
		s.handleRoomContext(w, r, roomID)
	case "close":
		s.handleRoomClose(w, r, roomID)
	case "transcript":
		s.handleTranscript(w, r, roomID)
	case "viewers":
		s.handleRoomViewers(w, r, roomID)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
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

func (s *sqlHTTP) adminAuthorized(r *http.Request) bool {
	adminToken := strings.TrimSpace(s.adminToken)
	if adminToken == "" {
		return false
	}
	if x := strings.TrimSpace(r.Header.Get("X-Admin-Token")); x != "" {
		return subtleConstantTimeEqual(x, adminToken)
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
		"agents_total":     out.Overview.AgentsTotal,
		"sessions_active":  out.Overview.SessionsActive,
		"rooms_open":       out.Overview.RoomsOpen,
		"rooms_active":     out.Overview.RoomsActive,
		"rooms_closed":     out.Overview.RoomsClosed,
		"rooms_purged":     out.Overview.RoomsPurged,
		"messages_total":   out.Overview.MessagesTotal,
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
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out, err := s.svc.GetPromptBundle(r.Context(), agentID, roomID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":            roomID,
		"bundle_hash":        out.BundleHash,
		"system_core_hash":   out.SystemCoreHash,
		"global_rules_hash":  out.GlobalRulesHash,
		"agent_rules_hash":   out.AgentRulesHash,
		"ordered_stack":      []string{"SYSTEM_CORE", "HARD_RULES_GLOBAL", "HARD_RULES_AGENT", "TASK_CONTEXT", "RECENT_MEMORY"},
		"prompt_bundle_text": out.Prompt,
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
		writeServiceErr(w, err)
		return
	}
	bundle, err := s.svc.GetPromptBundle(r.Context(), agentID, roomID)
	if err != nil {
		writeServiceErr(w, err)
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
	agentID, err := s.authAgentID(r.Context(), r)
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
		writeServiceErr(w, err)
		return
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
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out, err := s.svc.GetRoomState(r.Context(), agentID, roomID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":             out.Room.ID,
		"agent_a_id":     out.Room.AgentAID,
		"agent_b_id":     out.Room.AgentBID,
		"state":          out.Room.State,
		"turn_index":     out.Room.TurnIndex,
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
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	rm, err := s.svc.CloseRoom(r.Context(), agentID, roomID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id": rm.ID,
		"state":   rm.State,
	})
}

func (s *sqlHTTP) handleTranscript(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out, err := s.svc.Transcript(r.Context(), roomID, r.URL.Query().Get("human_code"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":   roomID,
		"state":     out.Room.State,
		"messages":  out.Messages,
		"closed_at": out.Room.ClosedAt,
		"purged_at": out.Room.PurgedAt,
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
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"viewer_token":   out.ViewerToken,
			"active_viewers": out.ActiveViewers,
		})
	case "heartbeat":
		out, err := s.svc.ViewerHeartbeat(r.Context(), roomID, req.ViewerToken)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"active_viewers": out.ActiveViewers})
	case "leave":
		out, err := s.svc.ViewerLeave(r.Context(), roomID, req.ViewerToken)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"active_viewers": out.ActiveViewers})
	default:
		writeError(w, http.StatusBadRequest, "unsupported op")
	}
}

func (s *sqlHTTP) authAgentID(ctx context.Context, r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		return "", a2a.ErrUnauthorized
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	return s.svc.AuthAgentID(ctx, token)
}

func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, a2a.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "invalid request")
	case errors.Is(err, a2a.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
	case errors.Is(err, a2a.ErrPolicyBlocked):
		writeError(w, http.StatusForbidden, "policy blocked")
	case errors.Is(err, a2a.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, a2a.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
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
