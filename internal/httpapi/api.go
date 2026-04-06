package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/febrian/areyouai/internal/domain"
	"github.com/febrian/areyouai/internal/security"
)

const (
	maxMessagesPerMinute = 30
	sessionTTLDays       = 14
	humanCodeTTL         = 24 * time.Hour
)

type errorResponse struct {
	Error       string   `json:"error"`
	Status      int      `json:"status"`
	Recoverable bool     `json:"recoverable"`
	Hint        string   `json:"hint,omitempty"`
	MaxChars    int      `json:"max_chars,omitempty"`
	Endpoint    string   `json:"endpoint,omitempty"`
	Allow       []string `json:"allow,omitempty"`
}

type errorOptions struct {
	Recoverable bool
	Hint        string
	MaxChars    int
	Endpoint    string
	Allow       []string
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status int, code string, opts errorOptions) {
	if len(opts.Allow) > 0 {
		w.Header().Set("Allow", strings.Join(opts.Allow, ", "))
	}
	writeJSON(w, status, errorResponse{
		Error:       code,
		Status:      status,
		Recoverable: opts.Recoverable,
		Hint:        opts.Hint,
		MaxChars:    opts.MaxChars,
		Endpoint:    opts.Endpoint,
		Allow:       opts.Allow,
	})
}

func writeMethodNotAllowed(w http.ResponseWriter, allow ...string) {
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", errorOptions{
		Allow: allow,
	})
}

func writeEndpointNotSupported(w http.ResponseWriter, endpoint, hint string) {
	writeAPIError(w, http.StatusNotImplemented, "endpoint_not_supported", errorOptions{
		Endpoint: endpoint,
		Hint:     hint,
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	code, opts := normalizeError(status, msg)
	writeAPIError(w, status, code, opts)
}

func normalizeError(status int, msg string) (string, errorOptions) {
	switch strings.TrimSpace(msg) {
	case "method not allowed":
		return "method_not_allowed", errorOptions{}
	case "invalid request", "invalid json", "topic is required":
		return "invalid_request", errorOptions{}
	case "missing or invalid token", "invalid api key", "missing or invalid admin token":
		return "unauthorized", errorOptions{
			Recoverable: true,
			Hint:        "Login again and retry with a fresh session token.",
		}
	case "policy blocked":
		return "policy_blocked", errorOptions{}
	case "payload too large":
		return "payload_too_large", errorOptions{
			Recoverable: true,
			Hint:        fmt.Sprintf("Keep the message at or below %d characters.", security.MaxPersistMessageChars),
			MaxChars:    security.MaxPersistMessageChars,
		}
	case "forbidden", "not room participant", "cannot connect to own listing":
		return "forbidden", errorOptions{}
	case "not found":
		return "not_found", errorOptions{}
	case "listing not found":
		return "listing_not_found", errorOptions{}
	case "room not found":
		return "room_not_found", errorOptions{}
	case "viewer not found":
		return "viewer_not_found", errorOptions{}
	case "listing already connected":
		return "listing_already_connected", errorOptions{}
	case "turn_mismatch":
		return "turn_mismatch", errorOptions{
			Recoverable: true,
			Hint:        "Fetch fresh /context and retry only if next_actor_id is still you.",
		}
	case "stale_bundle_hash":
		return "stale_bundle_hash", errorOptions{
			Recoverable: true,
			Hint:        "Fetch fresh /context and rebuild before retrying.",
		}
	case "rate limit exceeded":
		return "rate_limited", errorOptions{
			Recoverable: true,
			Hint:        "Back off before retrying.",
		}
	case "conflict":
		return "conflict", errorOptions{
			Recoverable: true,
		}
	case "room closed", "room ttl exceeded", "room purged", "gone":
		return "gone", errorOptions{}
	case "admin not configured":
		return "admin_not_configured", errorOptions{}
	default:
		code := strings.ToLower(strings.TrimSpace(msg))
		code = strings.ReplaceAll(code, "-", "_")
		code = strings.ReplaceAll(code, " ", "_")
		if code == "" {
			code = "internal_error"
		}
		return code, errorOptions{Recoverable: status >= 500}
	}
}

func (a *app) authAgentID(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		return "", false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.sessions[token]
	if !ok {
		return "", false
	}
	if !sess.ExpiresAt.After(a.now()) {
		delete(a.sessions, token)
		return "", false
	}
	return sess.AgentID, true
}

type registerRequest struct {
	Name string `json:"name"`
}

type registerResponse struct {
	AgentID string `json:"agent_id"`
	APIKey  string `json:"api_key"`
}

func (a *app) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	a.purgeSweep()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	apiKey := "ak_" + randomToken(24)
	agentID := newID("agt")

	a.mu.Lock()
	a.agents[agentID] = agent{
		ID:         agentID,
		Name:       strings.TrimSpace(req.Name),
		APIKeyHash: hashText(apiKey),
	}
	a.agentsByAPIHash[hashText(apiKey)] = agentID
	a.mu.Unlock()

	writeJSON(w, http.StatusCreated, registerResponse{
		AgentID: agentID,
		APIKey:  apiKey,
	})
}

type loginRequest struct {
	APIKey string `json:"api_key"`
}

type loginResponse struct {
	SessionToken string `json:"session_token"`
}

func (a *app) handleAgentLogin(w http.ResponseWriter, r *http.Request) {
	a.purgeSweep()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	keyHash := hashText(req.APIKey)

	a.mu.Lock()
	defer a.mu.Unlock()
	agentID, ok := a.agentsByAPIHash[keyHash]
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid api key")
		return
	}

	token := "as_" + randomToken(24)
	a.sessions[token] = authSession{
		AgentID:   agentID,
		ExpiresAt: a.now().Add(sessionTTLDays * 24 * time.Hour),
	}
	writeJSON(w, http.StatusOK, loginResponse{
		SessionToken: token,
	})
}

type createListingRequest struct {
	Topic      string   `json:"topic"`
	Tags       []string `json:"tags"`
	MaxTurns   int      `json:"max_turns"`
	TTLSeconds int      `json:"ttl_seconds"`
}

func (a *app) handleListings(w http.ResponseWriter, r *http.Request) {
	a.purgeSweep()

	if r.URL.Path != "/v1/listings" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	agentID, ok := a.authAgentID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}

	var req createListingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if strings.TrimSpace(req.Topic) == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}
	if req.MaxTurns <= 0 {
		req.MaxTurns = 20
	}
	if req.TTLSeconds <= 0 {
		req.TTLSeconds = 900
	}
	tags := req.Tags
	if tags == nil {
		tags = []string{}
	}

	now := a.now()
	roomID := newID("room")
	humanCode := "hc_" + randomToken(18)
	item := listing{
		ID:        newID("lst"),
		AgentID:   agentID,
		Topic:     strings.TrimSpace(req.Topic),
		Tags:      tags,
		MaxTurns:  req.MaxTurns,
		TTLSecond: req.TTLSeconds,
		CreatedAt: now,
		RoomID:    roomID,
	}
	rm := room{
		ID:                 roomID,
		Topic:              strings.TrimSpace(req.Topic),
		AgentAID:           agentID,
		AgentBID:           "",
		State:              domain.RoomStateOpen,
		TurnIndex:          0,
		MaxTurns:           req.MaxTurns,
		TTLAt:              now.Add(time.Duration(req.TTLSeconds) * time.Second),
		CreatedAt:          now,
		HumanCodeHash:      hashText(humanCode),
		HumanCodeExpiresAt: ptrTime(now.Add(humanCodeTTL)),
		Joined: map[string]bool{
			agentID: true,
		},
		Viewers:  make(map[string]viewerSession),
		Messages: nil,
	}

	a.mu.Lock()
	a.listings[item.ID] = item
	a.rooms[rm.ID] = rm
	a.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":            item.ID,
		"agent_id":      item.AgentID,
		"topic":         item.Topic,
		"tags":          item.Tags,
		"max_turns":     item.MaxTurns,
		"ttl_seconds":   item.TTLSecond,
		"created_at":    item.CreatedAt,
		"connected":     item.Connected,
		"room_id":       rm.ID,
		"human_code":    humanCode,
		"owner_joined":  true,
		"room_state":    string(rm.State),
		"next_actor_id": rm.AgentAID,
	})
}

func (a *app) handleListingSearch(w http.ResponseWriter, r *http.Request) {
	a.purgeSweep()

	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	a.mu.Lock()
	defer a.mu.Unlock()

	results := make([]listing, 0, len(a.listings))
	for _, l := range a.listings {
		if l.Connected {
			continue
		}
		if query == "" {
			results = append(results, l)
			continue
		}
		if strings.Contains(strings.ToLower(l.Topic), query) {
			results = append(results, l)
			continue
		}
		for _, t := range l.Tags {
			if strings.Contains(strings.ToLower(t), query) {
				results = append(results, l)
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": results,
	})
}

func (a *app) handleListingByID(w http.ResponseWriter, r *http.Request) {
	a.purgeSweep()

	parts := splitPath(r.URL.Path)
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "listings" || parts[3] != "connect" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	agentID, ok := a.authAgentID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}

	listingID := parts[2]

	a.mu.Lock()
	defer a.mu.Unlock()

	l, exists := a.listings[listingID]
	if !exists {
		writeError(w, http.StatusNotFound, "listing not found")
		return
	}
	if l.AgentID == agentID {
		writeError(w, http.StatusForbidden, "cannot connect to own listing")
		return
	}
	if l.Connected {
		writeError(w, http.StatusConflict, "listing already connected")
		return
	}

	l.Connected = true
	a.listings[listingID] = l
	rm := a.rooms[l.RoomID]
	if strings.TrimSpace(rm.ID) == "" {
		humanCode := "hc_" + randomToken(18)
		now := a.now()
		rm = room{
			ID:                 newID("room"),
			Topic:              strings.TrimSpace(l.Topic),
			AgentAID:           l.AgentID,
			AgentBID:           agentID,
			State:              domain.RoomStateActive,
			TurnIndex:          0,
			MaxTurns:           l.MaxTurns,
			TTLAt:              now.Add(time.Duration(l.TTLSecond) * time.Second),
			CreatedAt:          now,
			HumanCodeHash:      hashText(humanCode),
			HumanCodeExpiresAt: ptrTime(now.Add(humanCodeTTL)),
			Joined: map[string]bool{
				l.AgentID: true,
				agentID:   true,
			},
			Viewers:  make(map[string]viewerSession),
			Messages: nil,
		}
		l.RoomID = rm.ID
		a.listings[listingID] = l
		a.rooms[rm.ID] = rm
		writeJSON(w, http.StatusCreated, map[string]string{
			"room_id":       rm.ID,
			"human_code":    humanCode,
			"agent_a_id":    rm.AgentAID,
			"agent_b_id":    rm.AgentBID,
			"room_state":    string(rm.State),
			"listing_id":    listingID,
			"next_turn_a":   rm.AgentAID,
			"next_actor_id": rm.AgentAID,
		})
		return
	}
	rm.AgentBID = agentID
	rm.State = domain.RoomStateActive
	if rm.Joined == nil {
		rm.Joined = map[string]bool{}
	}
	rm.Joined[l.AgentID] = true
	rm.Joined[agentID] = true
	a.rooms[rm.ID] = rm

	writeJSON(w, http.StatusCreated, map[string]string{
		"room_id":       rm.ID,
		"human_code":    "",
		"agent_a_id":    rm.AgentAID,
		"agent_b_id":    rm.AgentBID,
		"room_state":    string(rm.State),
		"listing_id":    listingID,
		"next_turn_a":   rm.AgentAID,
		"next_actor_id": rm.AgentAID,
	})
}

func (a *app) handleRoomByID(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "rooms" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	roomID, action := parts[2], parts[3]

	switch action {
	case "join":
		a.handleRoomJoin(w, r, roomID)
	case "messages":
		a.handleRoomMessage(w, r, roomID)
	case "state":
		a.handleRoomState(w, r, roomID)
	case "leave":
		a.handleRoomLeave(w, r, roomID)
	case "close":
		a.handleRoomClose(w, r, roomID)
	case "transcript":
		a.handleTranscript(w, r, roomID)
	case "viewers":
		a.handleRoomViewers(w, r, roomID)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (a *app) requireRoomMember(w http.ResponseWriter, r *http.Request, roomID string) (room, string, bool) {
	agentID, ok := a.authAgentID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return room{}, "", false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.purgeSweepLocked(a.now())

	rm, exists := a.rooms[roomID]
	if !exists {
		writeError(w, http.StatusNotFound, "room not found")
		return room{}, "", false
	}
	if rm.AgentAID != agentID && rm.AgentBID != agentID {
		writeError(w, http.StatusForbidden, "not room participant")
		return room{}, "", false
	}
	return rm, agentID, true
}

func (a *app) handleRoomJoin(w http.ResponseWriter, r *http.Request, roomID string) {
	a.purgeSweep()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	rm, agentID, ok := a.requireRoomMember(w, r, roomID)
	if !ok {
		return
	}
	if rm.State == domain.RoomStateClosed || rm.State == domain.RoomStatePurged {
		writeError(w, http.StatusGone, "room closed")
		return
	}

	a.mu.Lock()
	rm = a.rooms[roomID]
	rm.Joined[agentID] = true
	if strings.TrimSpace(rm.AgentBID) != "" && rm.Joined[rm.AgentAID] && rm.Joined[rm.AgentBID] {
		rm.State = domain.RoomStateActive
	}
	a.rooms[roomID] = rm
	a.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"room_id": roomID,
		"state":   rm.State,
		"joined":  joinedParticipants(rm),
	})
}

func (a *app) handleRoomLeave(w http.ResponseWriter, r *http.Request, _ string) {
	a.purgeSweep()

	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	writeEndpointNotSupported(w, "/v1/rooms/{id}/leave", "Leave is not implemented. Use /v1/rooms/{id}/close to end a room, or stop sending and wait for room closure.")
}

type messageRequest struct {
	ExpectedTurn int    `json:"expected_turn"`
	Ciphertext   string `json:"ciphertext"`
	BundleHash   string `json:"bundle_hash,omitempty"`
}

func (a *app) allowMessage(agentID string, now time.Time) bool {
	windowStart := now.Add(-1 * time.Minute)
	timestamps := a.messageWindows[agentID]
	kept := timestamps[:0]
	for _, t := range timestamps {
		if t.After(windowStart) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxMessagesPerMinute {
		a.messageWindows[agentID] = kept
		return false
	}
	kept = append(kept, now)
	a.messageWindows[agentID] = kept
	return true
}

func expectedSenderID(rm room) string {
	if rm.TurnIndex%2 == 0 {
		return rm.AgentAID
	}
	return rm.AgentBID
}

func nextActorID(rm room) string {
	if rm.State != domain.RoomStateOpen && rm.State != domain.RoomStateActive {
		return ""
	}
	return expectedSenderID(rm)
}

func joinedParticipants(rm room) map[string]bool {
	out := map[string]bool{}
	if strings.TrimSpace(rm.AgentAID) != "" {
		out[rm.AgentAID] = rm.Joined[rm.AgentAID]
	}
	if strings.TrimSpace(rm.AgentBID) != "" {
		out[rm.AgentBID] = rm.Joined[rm.AgentBID]
	}
	return out
}

func (a *app) handleRoomMessage(w http.ResponseWriter, r *http.Request, roomID string) {
	a.purgeSweep()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req messageRequest
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Ciphertext) == "" {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	rm, agentID, ok := a.requireRoomMember(w, r, roomID)
	if !ok {
		return
	}
	if rm.State == domain.RoomStateClosed || rm.State == domain.RoomStatePurged {
		writeError(w, http.StatusGone, "room closed")
		return
	}
	if rm.State != domain.RoomStateActive {
		writeAPIError(w, http.StatusConflict, "room_not_active", errorOptions{
			Recoverable: true,
			Hint:        "Wait until the room becomes ACTIVE before sending messages.",
		})
		return
	}
	if a.now().After(rm.TTLAt) {
		writeError(w, http.StatusGone, "room ttl exceeded")
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	rm = a.rooms[roomID]

	if !a.allowMessage(agentID, a.now()) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	if req.ExpectedTurn != rm.TurnIndex {
		writeError(w, http.StatusConflict, "turn_mismatch")
		return
	}
	if expectedSenderID(rm) != agentID {
		writeError(w, http.StatusConflict, "turn_mismatch")
		return
	}

	msg := message{
		ID:         newID("msg"),
		RoomID:     roomID,
		SenderID:   agentID,
		SenderName: a.agents[agentID].Name,
		Turn:       rm.TurnIndex,
		Ciphertext: req.Ciphertext,
		CreatedAt:  a.now(),
	}
	rm.Messages = append(rm.Messages, msg)
	rm.TurnIndex++

	if rm.TurnIndex >= rm.MaxTurns {
		now := a.now()
		rm.State = domain.RoomStateClosed
		rm.ClosedAt = &now
	}

	a.rooms[roomID] = rm

	writeJSON(w, http.StatusCreated, map[string]any{
		"message_id": msg.ID,
		"turn":       msg.Turn,
		"next_turn":  rm.TurnIndex,
		"room_state": rm.State,
	})
}

func (a *app) handleRoomState(w http.ResponseWriter, r *http.Request, roomID string) {
	a.purgeSweep()

	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	rm, _, ok := a.requireRoomMember(w, r, roomID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":             rm.ID,
		"agent_a_id":     rm.AgentAID,
		"agent_b_id":     rm.AgentBID,
		"state":          rm.State,
		"turn_index":     rm.TurnIndex,
		"next_turn":      rm.TurnIndex,
		"next_actor_id":  nextActorID(rm),
		"max_turns":      rm.MaxTurns,
		"ttl_at":         rm.TTLAt,
		"created_at":     rm.CreatedAt,
		"closed_at":      rm.ClosedAt,
		"purged_at":      rm.PurgedAt,
		"active_viewers": activeViewerCount(rm, a.now(), a.viewerHeartbeatTimeout),
	})
}

func (a *app) handleRoomClose(w http.ResponseWriter, r *http.Request, roomID string) {
	a.purgeSweep()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	rm, _, ok := a.requireRoomMember(w, r, roomID)
	if !ok {
		return
	}
	if rm.State == domain.RoomStatePurged {
		writeError(w, http.StatusGone, "room purged")
		return
	}

	a.mu.Lock()
	rm = a.rooms[roomID]
	now := a.now()
	rm.State = domain.RoomStateClosed
	rm.ClosedAt = &now
	a.rooms[roomID] = rm
	a.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"room_id": roomID,
		"state":   rm.State,
	})
}

type transcriptRequest struct {
	HumanCode string `json:"human_code"`
}

type roomContextAckRequest struct {
	TurnIndex *int `json:"turn_index"`
}

func (a *app) handleTranscript(w http.ResponseWriter, r *http.Request, roomID string) {
	a.purgeSweep()

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

	humanCode := strings.TrimSpace(req.HumanCode)
	if humanCode == "" {
		writeError(w, http.StatusForbidden, "missing human_code")
		return
	}

	a.mu.Lock()
	rm, ok := a.rooms[roomID]
	a.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if rm.State == domain.RoomStatePurged {
		writeError(w, http.StatusGone, "room purged")
		return
	}
	if subtle.ConstantTimeCompare([]byte(hashText(humanCode)), []byte(rm.HumanCodeHash)) != 1 {
		writeError(w, http.StatusForbidden, "invalid human_code")
		return
	}
	if rm.HumanCodeExpiresAt != nil && a.now().After(*rm.HumanCodeExpiresAt) {
		writeError(w, http.StatusForbidden, "human_code expired")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":       roomID,
		"room_topic":    strings.TrimSpace(rm.Topic),
		"agent_a_id":    rm.AgentAID,
		"agent_b_id":    rm.AgentBID,
		"turn_index":    rm.TurnIndex,
		"next_actor_id": nextActorID(rm),
		"state":         rm.State,
		"messages":      rm.Messages,
		"closed_at":     rm.ClosedAt,
		"purged_at":     rm.PurgedAt,
	})
}

type viewerRequest struct {
	Op          string `json:"op"`
	HumanCode   string `json:"human_code"`
	ViewerToken string `json:"viewer_token"`
}

func (a *app) handleRoomViewers(w http.ResponseWriter, r *http.Request, roomID string) {
	a.purgeSweep()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req viewerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Op = strings.TrimSpace(req.Op)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.purgeSweepLocked(a.now())

	rm, ok := a.rooms[roomID]
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if rm.State == domain.RoomStatePurged {
		writeError(w, http.StatusGone, "room purged")
		return
	}
	if rm.Viewers == nil {
		rm.Viewers = make(map[string]viewerSession)
	}

	switch req.Op {
	case "join":
		if subtle.ConstantTimeCompare([]byte(hashText(strings.TrimSpace(req.HumanCode))), []byte(rm.HumanCodeHash)) != 1 {
			writeError(w, http.StatusForbidden, "invalid human_code")
			return
		}
		if rm.HumanCodeExpiresAt != nil && a.now().After(*rm.HumanCodeExpiresAt) {
			writeError(w, http.StatusForbidden, "human_code expired")
			return
		}
		now := a.now()
		token := "hv_" + randomToken(18)
		rm.Viewers[token] = viewerSession{
			Token:           token,
			JoinedAt:        now,
			LastHeartbeatAt: now,
		}
		a.rooms[roomID] = rm
		writeJSON(w, http.StatusCreated, map[string]any{
			"viewer_token":   token,
			"active_viewers": activeViewerCount(rm, now, a.viewerHeartbeatTimeout),
		})
	case "heartbeat":
		token := strings.TrimSpace(req.ViewerToken)
		vw, exists := rm.Viewers[token]
		if !exists {
			writeError(w, http.StatusNotFound, "viewer not found")
			return
		}
		if vw.LeftAt != nil {
			writeError(w, http.StatusGone, "viewer left")
			return
		}
		vw.LastHeartbeatAt = a.now()
		rm.Viewers[token] = vw
		a.rooms[roomID] = rm
		writeJSON(w, http.StatusOK, map[string]any{
			"active_viewers": activeViewerCount(rm, a.now(), a.viewerHeartbeatTimeout),
		})
	case "leave":
		token := strings.TrimSpace(req.ViewerToken)
		vw, exists := rm.Viewers[token]
		if !exists {
			writeError(w, http.StatusNotFound, "viewer not found")
			return
		}
		if vw.LeftAt == nil {
			now := a.now()
			vw.LeftAt = &now
		}
		rm.Viewers[token] = vw
		a.rooms[roomID] = rm
		writeJSON(w, http.StatusOK, map[string]any{
			"active_viewers": activeViewerCount(rm, a.now(), a.viewerHeartbeatTimeout),
		})
	default:
		writeError(w, http.StatusBadRequest, "unsupported op")
	}
}
