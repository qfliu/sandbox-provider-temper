package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

const goodKey = "sk_good"

// fakeAuth knows one key; "sk_boom" simulates a database error.
type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, key string) (store.User, store.KeyRef, error) {
	switch key {
	case goodKey:
		return store.User{ID: "u1", Name: "alice"}, store.KeyRef{ID: "k-good", Name: "laptop", Prefix: "sk_good"}, nil
	case "sk_boom":
		return store.User{}, store.KeyRef{}, errors.New("db down")
	default:
		return store.User{}, store.KeyRef{}, store.ErrNotFound
	}
}

func getWithAuth(h http.Handler, path, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMeWithValidKey(t *testing.T) {
	rec := getWithAuth(newTestHandler(fakeDB{}), "/v1/me", "Bearer "+goodKey)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var u store.User
	if err := json.NewDecoder(rec.Body).Decode(&u); err != nil || u.Name != "alice" {
		t.Fatalf("body = %+v, %v; want alice", u, err)
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	if rec := getWithAuth(newTestHandler(fakeDB{}), "/v1/me", "bearer "+goodKey); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestV1RejectsBadAuth(t *testing.T) {
	h := newTestHandler(fakeDB{})
	for name, header := range map[string]string{
		"no header":    "",
		"not bearer":   "Basic " + goodKey,
		"empty bearer": "Bearer ",
		"unknown key":  "Bearer sk_nope",
	} {
		rec := getWithAuth(h, "/v1/me", header)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: missing WWW-Authenticate header", name)
		}
	}
}

func TestUnknownV1PathNeedsAuthFirst(t *testing.T) {
	h := newTestHandler(fakeDB{})
	if rec := getWithAuth(h, "/v1/secret-route", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without key: status = %d, want 401 (don't reveal routes)", rec.Code)
	}
	if rec := getWithAuth(h, "/v1/secret-route", "Bearer "+goodKey); rec.Code != http.StatusNotFound {
		t.Fatalf("with key: status = %d, want 404", rec.Code)
	}
}

func TestAuthLookupErrorIs500(t *testing.T) {
	rec := getWithAuth(newTestHandler(fakeDB{}), "/v1/me", "Bearer sk_boom")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
