package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/febrian/areyouai/internal/domain"
)

type transitionRequest struct {
	Current domain.RoomState `json:"current"`
	Next    domain.RoomState `json:"next"`
}

func roomStateMachine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}

	var req transitionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if err := domain.TransitionState(req.Current, req.Next); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"result": "ok",
	})
}
