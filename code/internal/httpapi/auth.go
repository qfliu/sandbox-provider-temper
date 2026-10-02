package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// Authenticator resolves an API key to its owner (implemented by
// store.Store; faked in tests).
type Authenticator interface {
	UserByAPIKey(ctx context.Context, key string) (store.User, error)
}

type ctxKey int

const userKey ctxKey = 0

// userFrom returns the authenticated user for a request that passed
// requireAuth.
func userFrom(ctx context.Context) store.User {
	return ctx.Value(userKey).(store.User)
}

// requireAuth only lets requests with a valid "Authorization: Bearer <key>"
// header through, and puts the key's owner in the request context.
func requireAuth(log *slog.Logger, a Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The scheme name is case-insensitive per RFC 9110, so accept "bearer" too.
		scheme, key, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || key == "" {
			unauthorized(w, "missing API key: send Authorization: Bearer <key>")
			return
		}
		user, err := a.UserByAPIKey(r.Context(), key)
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w, "invalid or revoked API key")
			return
		}
		if err != nil {
			log.Error("api key lookup failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="sandbox-api"`)
	writeError(w, http.StatusUnauthorized, msg)
}

// handleMe returns the caller's own account, so an agent can check its key.
func handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, userFrom(r.Context()))
}
