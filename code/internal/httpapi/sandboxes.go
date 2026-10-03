package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// SandboxService is what the sandbox handlers need (implemented by
// sandbox.Service; faked in tests).
type SandboxService interface {
	Create(ctx context.Context, user store.User, req sandbox.CreateRequest) (store.Sandbox, error)
	Get(ctx context.Context, user store.User, id string) (store.Sandbox, error)
	List(ctx context.Context, user store.User, includeEnded bool) ([]store.Sandbox, error)
	Delete(ctx context.Context, user store.User, id string) (store.Sandbox, error)
	Exec(ctx context.Context, user store.User, id string, req sandbox.ExecRequest) (sshx.ExecResult, error)
	Upload(ctx context.Context, user store.User, id, path string, r io.Reader) (int64, error)
	Download(ctx context.Context, user store.User, id, path string, setSize func(int64), w io.Writer) error
}

const (
	maxRequestBody = 64 << 10  // 64 KiB is plenty for a JSON request
	maxUploadBody  = 100 << 20 // 100 MiB per file upload
)

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
		// Who created it: always the authenticated key, never the body
		// (the field has no JSON name, and unknown fields are rejected).
		req.CreatedBy = callerKey(r.Context())
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

// POST /v1/sandboxes/{id}/exec
func handleExec(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req sandbox.ExecRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		res, err := svc.Exec(r.Context(), userFrom(r.Context()), r.PathValue("id"), req)
		if err != nil {
			writeServiceError(log, w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// PUT /v1/sandboxes/{id}/files?path=… with the raw file as the body.
func handleUpload(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		body := http.MaxBytesReader(w, r.Body, maxUploadBody)
		n, err := svc.Upload(r.Context(), userFrom(r.Context()), r.PathValue("id"), path, body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "file larger than 100 MiB")
			return
		}
		if err != nil {
			writeServiceError(log, w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": sshx.ResolvePath(path), "size": n})
	}
}

// GET /v1/sandboxes/{id}/files?path=… streams the raw file back.
func handleDownload(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := false
		setSize := func(n int64) {
			started = true
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
			w.WriteHeader(http.StatusOK)
		}
		err := svc.Download(r.Context(), userFrom(r.Context()), r.PathValue("id"),
			r.URL.Query().Get("path"), setSize, w)
		if err == nil {
			return
		}
		if started {
			// Headers are already sent; the client sees a short body.
			log.Error("download interrupted", "err", err)
			return
		}
		writeServiceError(log, w, err)
	}
}

// writeServiceError maps service errors to HTTP status codes. Unexpected
// errors are logged and hidden behind a generic 500.
func writeServiceError(log *slog.Logger, w http.ResponseWriter, err error) {
	status, msg := serviceError(log, err)
	writeError(w, status, msg)
}

// serviceError turns a service error into an HTTP status and a message safe
// to show the caller (shared by the JSON API and the dashboard).
func serviceError(log *slog.Logger, err error) (int, string) {
	var verr *sandbox.ValidationError
	var notRunning *sandbox.NotRunningError
	switch {
	case errors.As(err, &verr):
		return http.StatusBadRequest, verr.Msg
	case errors.As(err, &notRunning):
		return http.StatusConflict, notRunning.Error()
	case errors.Is(err, sandbox.ErrUnreachable):
		return http.StatusBadGateway, "sandbox unreachable, try again"
	case errors.Is(err, sshx.ErrNotFound):
		return http.StatusNotFound, "file not found"
	case errors.Is(err, sshx.ErrIsDir):
		return http.StatusBadRequest, "path is a directory"
	case errors.Is(err, fs.ErrPermission):
		// The sandbox's own file permissions said no (we act as `user`).
		return http.StatusForbidden, "permission denied inside the sandbox"
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusBadRequest, "path does not exist or its parent is not a directory"
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "sandbox not found"
	case errors.Is(err, store.ErrQuotaExceeded):
		return http.StatusTooManyRequests, "sandbox quota exceeded: delete a sandbox first"
	case errors.Is(err, sandbox.ErrUnavailable):
		return http.StatusServiceUnavailable, "cluster busy or unavailable, try again"
	default:
		log.Error("request failed", "err", err)
		return http.StatusInternalServerError, "internal error"
	}
}
