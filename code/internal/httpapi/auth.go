package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// Authenticator resolves an API key to its owner and the key itself
// (implemented by store.Store; faked in tests).
type Authenticator interface {
	Authenticate(ctx context.Context, key string) (store.User, store.KeyRef, error)
}

type ctxKey int

const (
	userKey ctxKey = iota
	apiKeyKey
)

// userFrom returns the authenticated user for a request that passed
// requireAuth or requireSession.
func userFrom(ctx context.Context) store.User {
	return ctx.Value(userKey).(store.User)
}

// keyFrom returns the API key the request was authenticated with.
func keyFrom(ctx context.Context) store.KeyRef {
	k, _ := ctx.Value(apiKeyKey).(store.KeyRef)
	return k
}

// callerKey is the request's key as a sandbox's "created by", or nil when
// the request wasn't authenticated with a key (shouldn't happen behind
// requireAuth/requireSession).
func callerKey(ctx context.Context) *store.KeyRef {
	k := keyFrom(ctx)
	if k.ID == "" {
		return nil
	}
	return &k
}

// withCaller records who made the request: the user and their key.
func withCaller(ctx context.Context, u store.User, k store.KeyRef) context.Context {
	return context.WithValue(context.WithValue(ctx, userKey, u), apiKeyKey, k)
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
		user, apiKey, err := a.Authenticate(r.Context(), key)
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w, "invalid or revoked API key")
			return
		}
		if err != nil {
			log.Error("api key lookup failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), user, apiKey)))
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
