package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// fakeSandboxes returns err from every call when set.
type fakeSandboxes struct{ err error }

func (f fakeSandboxes) Create(_ context.Context, _ store.User, req sandbox.CreateRequest) (store.Sandbox, error) {
	return store.Sandbox{ID: "sb-1", Image: req.Image, Status: "pending"}, f.err
}
func (f fakeSandboxes) Get(_ context.Context, _ store.User, id string) (store.Sandbox, error) {
	return store.Sandbox{ID: id}, f.err
}
func (f fakeSandboxes) List(context.Context, store.User, bool) ([]store.Sandbox, error) {
	return []store.Sandbox{}, f.err
}
func (f fakeSandboxes) Delete(_ context.Context, _ store.User, id string) (store.Sandbox, error) {
	return store.Sandbox{ID: id, Status: "deleted"}, f.err
}

func sandboxHandler(err error) http.Handler {
	return NewHandler(Deps{
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:        fakeDB{},
		Auth:      fakeAuth{},
		Sandboxes: fakeSandboxes{err: err},
	})
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+goodKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSandboxRoutes(t *testing.T) {
	h := sandboxHandler(nil)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1/sandboxes", `{"image":"ubuntu-dev","ssh_public_key":"k"}`, 201},
		{"GET", "/v1/sandboxes", "", 200},
		{"GET", "/v1/sandboxes/sb-1", "", 200},
		{"DELETE", "/v1/sandboxes/sb-1", "", 200},
		{"POST", "/v1/sandboxes", `not json`, 400},
		{"POST", "/v1/sandboxes", `{"image":"ubuntu-dev","ttl":60}`, 400}, // unknown field
		{"PUT", "/v1/sandboxes/sb-1", "", 405},
	} {
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s %s %s: status = %d, want %d (body %s)", tc.method, tc.path, tc.body, rec.Code, tc.want, rec.Body)
		}
	}
}

func TestSandboxErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&sandbox.ValidationError{Msg: "bad"}, 400},
		{store.ErrNotFound, 404},
		{store.ErrQuotaExceeded, 429},
		{sandbox.ErrUnavailable, 503},
		{io.ErrUnexpectedEOF, 500},
	} {
		rec := do(sandboxHandler(tc.err), "POST", "/v1/sandboxes", `{"image":"ubuntu-dev"}`)
		if rec.Code != tc.want {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}

func TestSandboxRoutesNeedAuth(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	sandboxHandler(nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
