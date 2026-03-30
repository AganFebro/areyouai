package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/febrian/areyouai/internal/repository"
	"github.com/febrian/areyouai/internal/service/a2a"
)

type sqlHTTP struct {
	svc *a2a.Service
}

func newSQLHTTP(store repository.Store, opts options) *sqlHTTP {
	return &sqlHTTP{
		svc: a2a.New(store, a2a.Options{
			ViewerHeartbeatTimeout: opts.ViewerHeartbeatTimeout,
			ClosedRoomGraceDelay:   opts.ClosedRoomGraceDelay,
			MaxClosedRetention:     opts.MaxClosedRetention,
		}),
	}
}

func (s *sqlHTTP) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"room_id": roomID,
		"state":   state,
		"joined":  joined,
	})
}

func (s *sqlHTTP) handleRoomMessage(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := s.authAgentID(r.Context(), r)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	var req messageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	msg, state, nextTurn, err := s.svc.SendMessage(r.Context(), agentID, roomID, req.ExpectedTurn, req.Ciphertext)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"message_id": msg.ID,
		"turn":       msg.Turn,
		"next_turn":  nextTurn,
		"room_state": state,
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
