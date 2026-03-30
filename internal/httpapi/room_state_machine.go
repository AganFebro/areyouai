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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req transitionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if err := domain.TransitionState(req.Current, req.Next); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"result": "ok",
	})
}
