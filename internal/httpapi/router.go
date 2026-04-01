package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
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
	return NewRouterWithStoreAndAdmin(
		store,
		viewerHeartbeatTimeout,
		closedRoomGraceDelay,
		maxClosedRetention,
		"",
	)
}

func NewRouterWithStoreAndAdmin(
	store repository.Store,
	viewerHeartbeatTimeout time.Duration,
	closedRoomGraceDelay time.Duration,
	maxClosedRetention time.Duration,
	adminToken string,
) http.Handler {
	opts := options{
		ViewerHeartbeatTimeout: viewerHeartbeatTimeout,
		ClosedRoomGraceDelay:   closedRoomGraceDelay,
		MaxClosedRetention:     maxClosedRetention,
		AdminToken:             strings.TrimSpace(adminToken),
	}
	app := newApp(opts)
	sqlMode := store != nil
	sqlHandlers := newSQLHTTP(store, opts)
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/v1/mode", modeInfo(sqlMode))
	mux.HandleFunc("/skill.md", serveSkillMD)
	mux.HandleFunc("/nodejs_loop.md", serveNodeJSLoopMD)
	mux.HandleFunc("/python_loop.md", servePythonLoopMD)
	mux.HandleFunc("/v1/rooms/state-machine", roomStateMachine)
	if sqlMode {
		mux.HandleFunc("/v1/agent/register", sqlHandlers.handleAgentRegister)
		mux.HandleFunc("/v1/agent/login", sqlHandlers.handleAgentLogin)
		mux.HandleFunc("/v1/admin/", sqlHandlers.handleAdmin)
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

	return withAccessLogs(withSecurityHeaders(withCORS(mux)), store)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

func modeInfo(sqlMode bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mode := "polling"
		pollInterval := 3000
		if sqlMode {
			mode = "sse"
			// Polling fallback interval if SSE client is unavailable.
			pollInterval = 5000
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":             mode,
			"poll_interval_ms": pollInterval,
		})
	}
}

func serveSkillMD(w http.ResponseWriter, r *http.Request) {
	serveMarkdownFile(w, r, strings.TrimSpace(os.Getenv("SKILL_MD_PATH")), "skill.md")
}

func serveNodeJSLoopMD(w http.ResponseWriter, r *http.Request) {
	serveMarkdownFile(w, r, strings.TrimSpace(os.Getenv("NODEJS_LOOP_MD_PATH")), "nodejs_loop.md")
}

func servePythonLoopMD(w http.ResponseWriter, r *http.Request) {
	serveMarkdownFile(w, r, strings.TrimSpace(os.Getenv("PYTHON_LOOP_MD_PATH")), "python_loop.md")
}

func serveMarkdownFile(w http.ResponseWriter, r *http.Request, primaryPath, fallbackName string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if strings.TrimSpace(fallbackName) == "" {
		http.Error(w, "markdown not found", http.StatusNotFound)
		return
	}

	if strings.TrimSpace(primaryPath) == "" {
		primaryPath = fallbackName
	}

	body, err := readMarkdownFile(primaryPath, fallbackName)
	if err != nil {
		http.Error(w, fallbackName+" not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

func readMarkdownFile(primaryPath, fallbackName string) ([]byte, error) {
	paths := []string{primaryPath, "../" + fallbackName, "../../" + fallbackName}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		body, err := os.ReadFile(path)
		if err == nil {
			return body, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, os.ErrNotExist
}

func readSkillMD(primaryPath string) ([]byte, error) {
	return readMarkdownFile(primaryPath, "skill.md")
}

func withCORS(next http.Handler) http.Handler {
	allowedOrigins := buildAllowedOrigins()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isAllowedOrigin(origin, allowedOrigins) {
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

func buildAllowedOrigins() []string {
	if env := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS")); env != "" {
		parts := strings.Split(env, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if v := strings.TrimSpace(p); v != "" {
				out = append(out, v)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{
		"http://localhost:3000",
		"http://127.0.0.1:3000",
		"https://areyouai.fun",
		"https://www.areyouai.fun",
	}
}

func isAllowedOrigin(origin string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(origin, a) {
			return true
		}
	}
	return false
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
