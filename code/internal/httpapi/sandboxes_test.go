package httpapi

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// fakeSandboxes returns err from every call when set.
type fakeSandboxes struct{ err error }

func (f fakeSandboxes) Create(_ context.Context, _ store.User, req sandbox.CreateRequest) (store.Sandbox, error) {
	return store.Sandbox{ID: "sb-1", Image: req.Image, Runtime: req.Runtime, Status: "pending"}, f.err
}
func (f fakeSandboxes) Get(_ context.Context, _ store.User, id string) (store.Sandbox, error) {
	return store.Sandbox{ID: id}, f.err
}
func (f fakeSandboxes) List(context.Context, store.User, bool) ([]store.Sandbox, error) {
	return []store.Sandbox{}, f.err
}
func (f fakeSandboxes) ListPage(context.Context, store.User, bool, int, int) ([]store.Sandbox, int, error) {
	return []store.Sandbox{}, 0, f.err
}
func (f fakeSandboxes) Delete(_ context.Context, _ store.User, id string) (store.Sandbox, error) {
	return store.Sandbox{ID: id, Status: "deleted"}, f.err
}
func (f fakeSandboxes) Exec(_ context.Context, _ store.User, _ string, req sandbox.ExecRequest) (sshx.ExecResult, error) {
	return sshx.ExecResult{Stdout: "ran: " + req.Command}, f.err
}
func (f fakeSandboxes) Upload(_ context.Context, _ store.User, _, _ string, r io.Reader) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return io.Copy(io.Discard, r)
}
func (f fakeSandboxes) Download(_ context.Context, _ store.User, _, _ string, setSize func(int64), w io.Writer) error {
	if f.err != nil {
		return f.err
	}
	setSize(5)
	_, err := w.Write([]byte("hello"))
	return err
}

func (f fakeSandboxes) OpenPort(context.Context, store.User, string, int) (sandbox.PortDialer, func(), error) {
	return nil, nil, f.err
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
		{"POST", "/v1/sandboxes/sb-1/exec", `{"command":"ls"}`, 200},
		{"POST", "/v1/sandboxes/sb-1/exec", `{"cmd":"ls"}`, 400}, // unknown field
		{"PUT", "/v1/sandboxes/sb-1/files?path=a.txt", "data", 200},
		{"GET", "/v1/sandboxes/sb-1/files?path=a.txt", "", 200},
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
		{&sandbox.NotRunningError{Status: "pending"}, 409},
		{sandbox.ErrUnreachable, 502},
		{sshx.ErrNotFound, 404},
		{fmt.Errorf("open /etc/x: %w", fs.ErrPermission), 403},
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

func TestDownloadStreamsWithLength(t *testing.T) {
	rec := do(sandboxHandler(nil), "GET", "/v1/sandboxes/sb-1/files?path=a.txt", "")
	if rec.Code != 200 || rec.Body.String() != "hello" || rec.Header().Get("Content-Length") != "5" {
		t.Fatalf("got %d %q length=%q", rec.Code, rec.Body, rec.Header().Get("Content-Length"))
	}
}

func TestUploadTooLarge(t *testing.T) {
	big := strings.Repeat("x", maxUploadBody+1)
	if rec := do(sandboxHandler(nil), "PUT", "/v1/sandboxes/sb-1/files?path=a", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// The runtime in a /v1 create reaches the service and comes back (D65).
func TestCreateRuntimeField(t *testing.T) {
	rec := do(sandboxHandler(nil), http.MethodPost, "/v1/sandboxes",
		`{"image":"ubuntu-dev","ssh_public_key":"ssh-ed25519 AAAA","runtime":"kata"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"runtime":"kata"`) {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
}

// A client that hangs up (cancelled request, upload cut short) gets a quiet
// 499, not a 500 logged as a server error.
func TestServiceErrorClientGone(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, err := range []error{context.Canceled, fmt.Errorf("upload: %w", sshx.ErrClientGone)} {
		if status, _ := serviceError(log, err); status != 499 {
			t.Errorf("%v: status %d, want 499", err, status)
		}
	}
}
