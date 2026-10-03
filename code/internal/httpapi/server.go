// Package httpapi is the control plane's HTTP layer: routing, JSON
// responses and middleware. Business logic lives in other packages.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// Pinger reports whether a dependency (Postgres) is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the services the HTTP handlers need.
type Deps struct {
	Log       *slog.Logger
	DB        Pinger
	Auth      Authenticator
	Sandboxes SandboxService
	// SSHGateway is the gateway's public "host:port", shown in the
	// dashboard's ssh command (optional).
	SSHGateway string
	// Keys manages users' API keys from the dashboard.
	Keys KeyService
	// Admin lists all users and sandboxes for the dashboard's admin view.
	Admin AdminService
}

// AdminService reads platform-wide data for admins (implemented by
// store.Store; faked in tests). Callers must check the user is an admin.
type AdminService interface {
	ListUsers(ctx context.Context) ([]store.UserSummary, error)
	ListAllSandboxes(ctx context.Context, includeEnded bool) ([]store.OwnedSandbox, error)
}

// KeyService lists, issues and revokes a user's API keys (implemented by
// store.Store; faked in tests).
type KeyService interface {
	ListAPIKeys(ctx context.Context, userID string) ([]store.APIKey, error)
	CreateAPIKey(ctx context.Context, userID, name string) (string, store.APIKey, error)
	RevokeAPIKey(ctx context.Context, userID, keyID string) error
}

// NewHandler returns the API's root handler with all routes registered.
//
// /healthz and /readyz are public (Kubernetes probes them). Everything under
// /v1/ requires an API key, including unknown /v1 paths, so the API doesn't
// reveal which routes exist to unauthenticated callers. /ui is the dashboard
// (D13, D40), signed in with an API key held in a session cookie.
func NewHandler(d Deps) http.Handler {
	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/me", handleMe)
	v1.HandleFunc("POST /v1/sandboxes", handleCreateSandbox(d.Log, d.Sandboxes))
	v1.HandleFunc("GET /v1/sandboxes", handleListSandboxes(d.Log, d.Sandboxes))
	v1.HandleFunc("GET /v1/sandboxes/{id}", handleGetSandbox(d.Log, d.Sandboxes))
	v1.HandleFunc("DELETE /v1/sandboxes/{id}", handleDeleteSandbox(d.Log, d.Sandboxes))
	v1.HandleFunc("POST /v1/sandboxes/{id}/exec", handleExec(d.Log, d.Sandboxes))
	v1.HandleFunc("PUT /v1/sandboxes/{id}/files", handleUpload(d.Log, d.Sandboxes))
	v1.HandleFunc("GET /v1/sandboxes/{id}/files", handleDownload(d.Log, d.Sandboxes))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(d.Log, d.DB))
	mux.Handle("/v1/", requireAuth(d.Log, d.Auth, v1))
	ui := newUI(d.Log, d.Auth, d.Sandboxes, d.Keys, d.Admin, d.SSHGateway).routes()
	mux.Handle("/ui", ui)
	mux.Handle("/ui/", ui)
	return logRequests(d.Log, mux)
}

// handleHealthz reports liveness: the process is up. Kubernetes restarts the
// pod if this fails, so it deliberately doesn't check dependencies.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports readiness: the API can serve requests because the
// database is reachable. Kubernetes stops sending traffic while it fails.
func handleReadyz(log *slog.Logger, db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			log.Warn("readiness check failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

// writeError sends {"error": "..."}, the shape of every API error.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // this is an API, not HTML: keep "<key>" readable
	_ = enc.Encode(v)
}

// logRequests logs one line per request with its method, path and status.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status)
	})
}

// statusRecorder captures the status code a handler writes.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
