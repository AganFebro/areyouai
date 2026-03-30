package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/febrian/areyouai/internal/repository"
)

func NewRouter() http.Handler {
	return NewRouterWithStore(nil, time.Duration(0), time.Duration(0), time.Duration(0))
}

func NewRouterWithOptions(
	viewerHeartbeatTimeout time.Duration,
	closedRoomGraceDelay time.Duration,
	maxClosedRetention time.Duration,
) http.Handler {
	return NewRouterWithStore(nil, viewerHeartbeatTimeout, closedRoomGraceDelay, maxClosedRetention)
}

func NewRouterWithStore(
	store repository.Store,
	viewerHeartbeatTimeout time.Duration,
	closedRoomGraceDelay time.Duration,
	maxClosedRetention time.Duration,
) http.Handler {
	opts := options{
		ViewerHeartbeatTimeout: viewerHeartbeatTimeout,
		ClosedRoomGraceDelay:   closedRoomGraceDelay,
		MaxClosedRetention:     maxClosedRetention,
	}
	app := newApp(opts)
	sqlMode := store != nil
	sqlHandlers := newSQLHTTP(store, opts)
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/v1/rooms/state-machine", roomStateMachine)
	if sqlMode {
		mux.HandleFunc("/v1/agent/register", sqlHandlers.handleAgentRegister)
		mux.HandleFunc("/v1/agent/login", sqlHandlers.handleAgentLogin)
		mux.HandleFunc("/v1/listings", sqlHandlers.handleListings)
		mux.HandleFunc("/v1/listings/search", sqlHandlers.handleListingSearch)
		mux.HandleFunc("/v1/listings/", sqlHandlers.handleListingByID)
		mux.HandleFunc("/v1/rooms/", sqlHandlers.handleRoomByID)
	} else {
		mux.HandleFunc("/v1/agent/register", app.handleAgentRegister)
		mux.HandleFunc("/v1/agent/login", app.handleAgentLogin)
		mux.HandleFunc("/v1/listings", app.handleListings)
		mux.HandleFunc("/v1/listings/search", app.handleListingSearch)
		mux.HandleFunc("/v1/listings/", app.handleListingByID)
		mux.HandleFunc("/v1/rooms/", app.handleRoomByID)
	}

	return withCORS(mux)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAllowedOrigin(origin string) bool {
	allowed := []string{
		"http://localhost:3000",
		"http://127.0.0.1:3000",
	}
	for _, a := range allowed {
		if strings.EqualFold(origin, a) {
			return true
		}
	}
	return false
}
