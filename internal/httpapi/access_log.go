package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/febrian/areyouai/internal/repository"
)

type loggingResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *loggingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (w *loggingResponseWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func withAccessLogs(next http.Handler, store repository.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := requestIDFromHeaders(r)
		w.Header().Set("X-Request-Id", requestID)

		lw := &loggingResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}
		next.ServeHTTP(lw, r)

		path := strings.TrimSpace(r.URL.Path)
		if path == "" {
			path = "/"
		}
		routeName := requestRouteName(r.Method, path)
		ip := remoteIP(r.RemoteAddr)
		if ip == "" {
			ip = "unknown"
		}
		query := sanitizeQueryString(r.URL.RawQuery)
		userAgent := strings.TrimSpace(r.UserAgent())
		if len(userAgent) > 256 {
			userAgent = userAgent[:256]
		}
		durationMS := int(time.Since(start).Milliseconds())
		if durationMS < 0 {
			durationMS = 0
		}
		authPresent := strings.TrimSpace(r.Header.Get("Authorization")) != "" ||
			strings.TrimSpace(r.Header.Get("X-Admin-Token")) != ""

		log.Printf(
			"api_request request_id=%s method=%s path=%s route=%s status=%d duration_ms=%d ip=%s bytes=%d auth_present=%t",
			requestID,
			r.Method,
			path,
			routeName,
			lw.status,
			durationMS,
			ip,
			lw.bytes,
			authPresent,
		)

		if store == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := store.AppendAPIRequestLog(ctx, repository.AppendAPIRequestLogInput{
			RequestID:    requestID,
			Method:       r.Method,
			Path:         path,
			RouteName:    routeName,
			Query:        query,
			StatusCode:   lw.status,
			DurationMS:   durationMS,
			RemoteIP:     ip,
			UserAgent:    userAgent,
			BytesWritten: lw.bytes,
			AuthPresent:  authPresent,
		}); err != nil {
			log.Printf("api_request_db_log_failed request_id=%s err=%v", requestID, err)
		}
	})
}

func requestRouteName(method, path string) string {
	switch {
	case method == http.MethodGet && strings.HasSuffix(path, "/context"):
		return "room_context"
	case method == http.MethodPost && strings.HasSuffix(path, "/context/ack"):
		return "room_context_ack"
	default:
		return ""
	}
}

func requestIDFromHeaders(r *http.Request) string {
	candidates := []string{
		r.Header.Get("X-Request-Id"),
		r.Header.Get("X-Request-ID"),
		r.Header.Get("X-Correlation-Id"),
		r.Header.Get("X-Correlation-ID"),
	}
	for _, item := range candidates {
		id := strings.TrimSpace(item)
		if id != "" {
			if len(id) > 64 {
				return id[:64]
			}
			return id
		}
	}
	return "req_" + randomHex(8)
}

func randomHex(numBytes int) string {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

func sanitizeQueryString(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	sanitized := url.Values{}
	for key, vals := range values {
		if isSensitiveQueryKey(key) {
			sanitized.Set(key, "[REDACTED]")
			continue
		}
		if len(vals) == 0 {
			sanitized.Set(key, "")
			continue
		}
		cleanVals := make([]string, 0, len(vals))
		for _, v := range vals {
			cleanVals = append(cleanVals, clipString(strings.TrimSpace(v), 128))
		}
		sanitized[key] = cleanVals
	}
	encoded := sanitized.Encode()
	return clipString(encoded, 512)
}

func isSensitiveQueryKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "human_code", "viewer_token", "api_key", "session_token", "token", "authorization", "admin_token":
		return true
	default:
		return false
	}
}

func clipString(in string, max int) string {
	if max <= 0 || len(in) <= max {
		return in
	}
	return in[:max] + "..."
}
