package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/febrian/areyouai/internal/domain"
)

const (
	maxMessagesPerMinute = 30
	sessionTTLDays       = 14
)

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.APIKey) == "" {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	item := listing{
		ID:        newID("lst"),
		AgentID:   agentID,
		Topic:     strings.TrimSpace(req.Topic),
		Tags:      req.Tags,
		MaxTurns:  req.MaxTurns,
		TTLSecond: req.TTLSeconds,
		CreatedAt: a.now(),
	}

	a.mu.Lock()
	a.listings[item.ID] = item
	a.mu.Unlock()

	writeJSON(w, http.StatusCreated, item)
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

	humanCode := "hc_" + randomToken(18)
	now := a.now()
	room := room{
		ID:            newID("room"),
		AgentAID:      l.AgentID,
		AgentBID:      agentID,
		State:         domain.RoomStateOpen,
		TurnIndex:     0,
		MaxTurns:      l.MaxTurns,
		TTLAt:         now.Add(time.Duration(l.TTLSecond) * time.Second),
		CreatedAt:     now,
		HumanCodeHash: hashText(humanCode),
		Joined:        make(map[string]bool),
		Viewers:       make(map[string]viewerSession),
		Messages:      nil,
	}

	l.Connected = true
	a.listings[listingID] = l
	a.rooms[room.ID] = room

	writeJSON(w, http.StatusCreated, map[string]string{
		"room_id":     room.ID,
		"human_code":  humanCode,
		"agent_a_id":  room.AgentAID,
		"agent_b_id":  room.AgentBID,
		"room_state":  string(room.State),
		"listing_id":  listingID,
		"next_turn_a": room.AgentAID,
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
	if rm.Joined[rm.AgentAID] && rm.Joined[rm.AgentBID] {
		rm.State = domain.RoomStateActive
	}
	a.rooms[roomID] = rm
	a.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"room_id": roomID,
		"state":   rm.State,
		"joined":  rm.Joined,
	})
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

func (a *app) handleRoomMessage(w http.ResponseWriter, r *http.Request, roomID string) {
	a.purgeSweep()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req messageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Ciphertext) == "" {
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
		writeError(w, http.StatusConflict, "turn conflict")
		return
	}
	if expectedSenderID(rm) != agentID {
		writeError(w, http.StatusConflict, "not your turn")
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

func (a *app) handleTranscript(w http.ResponseWriter, r *http.Request, roomID string) {
	a.purgeSweep()

	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	humanCode := strings.TrimSpace(r.URL.Query().Get("human_code"))
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

	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":   roomID,
		"state":     rm.State,
		"messages":  rm.Messages,
		"closed_at": rm.ClosedAt,
		"purged_at": rm.PurgedAt,
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
