package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeDB lets tests choose whether the database "is reachable".
type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestHandler(db Pinger) http.Handler {
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), db)
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	rec := get(newTestHandler(fakeDB{}), "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestHealthzIgnoresDatabase(t *testing.T) {
	rec := get(newTestHandler(fakeDB{err: errors.New("down")}), "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with the database down", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	if rec := get(newTestHandler(fakeDB{}), "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("database up: status = %d, want 200", rec.Code)
	}
	if rec := get(newTestHandler(fakeDB{err: errors.New("down")}), "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("database down: status = %d, want 503", rec.Code)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	if rec := get(newTestHandler(fakeDB{}), "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
