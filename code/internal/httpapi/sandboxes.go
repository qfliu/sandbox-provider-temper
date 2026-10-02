package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// SandboxService is what the sandbox handlers need (implemented by
// sandbox.Service; faked in tests).
type SandboxService interface {
	Create(ctx context.Context, user store.User, req sandbox.CreateRequest) (store.Sandbox, error)
	Get(ctx context.Context, user store.User, id string) (store.Sandbox, error)
	List(ctx context.Context, user store.User, includeEnded bool) ([]store.Sandbox, error)
	Delete(ctx context.Context, user store.User, id string) (store.Sandbox, error)
}

const maxRequestBody = 64 << 10 // 64 KiB is plenty for a create request

// POST /v1/sandboxes
func handleCreateSandbox(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req sandbox.CreateRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields() // catch typos like "ttl" instead of "ttl_seconds"
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		sb, err := svc.Create(r.Context(), userFrom(r.Context()), req)
		if err != nil {
			writeServiceError(log, w, err)
			return
		}
		writeJSON(w, http.StatusCreated, sb)
	}
}

// GET /v1/sandboxes[?include_ended=true]
func handleListSandboxes(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		includeEnded := r.URL.Query().Get("include_ended") == "true"
		list, err := svc.List(r.Context(), userFrom(r.Context()), includeEnded)
		if err != nil {
			writeServiceError(log, w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sandboxes": list})
	}
}

// GET /v1/sandboxes/{id}
func handleGetSandbox(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sb, err := svc.Get(r.Context(), userFrom(r.Context()), r.PathValue("id"))
		if err != nil {
			writeServiceError(log, w, err)
			return
		}
		writeJSON(w, http.StatusOK, sb)
	}
}

// DELETE /v1/sandboxes/{id}
func handleDeleteSandbox(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sb, err := svc.Delete(r.Context(), userFrom(r.Context()), r.PathValue("id"))
		if err != nil {
			writeServiceError(log, w, err)
			return
		}
		writeJSON(w, http.StatusOK, sb)
	}
}

// writeServiceError maps service errors to HTTP status codes. Unexpected
// errors are logged and hidden behind a generic 500.
func writeServiceError(log *slog.Logger, w http.ResponseWriter, err error) {
	var verr *sandbox.ValidationError
	switch {
	case errors.As(err, &verr):
		writeError(w, http.StatusBadRequest, verr.Msg)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "sandbox not found")
	case errors.Is(err, store.ErrQuotaExceeded):
		writeError(w, http.StatusTooManyRequests, "sandbox quota exceeded: delete a sandbox first")
	case errors.Is(err, sandbox.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "could not start sandbox, try again")
	default:
		log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
