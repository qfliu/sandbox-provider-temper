package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// portSandboxes stands in for the service: OpenPort dials a local test
// server, as the real one dials 127.0.0.1:port inside the sandbox over SSH.
type portSandboxes struct {
	fakeSandboxes
	addr     string // where the "sandbox's" server listens; "" = nothing does
	opened   []int
	released int
}

func (f *portSandboxes) OpenPort(_ context.Context, _ store.User, id string, port int) (sandbox.PortDialer, func(), error) {
	if id != "sb-1" {
		return nil, nil, store.ErrNotFound
	}
	if port < 1 || port > 65535 {
		return nil, nil, &sandbox.ValidationError{Msg: "port must be between 1 and 65535"}
	}
	f.opened = append(f.opened, port)
	addr := f.addr
	if addr == "" {
		addr = "127.0.0.1:1" // refuses connections
	}
	return func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}, func() {
			f.released++
		}, nil
}

func portHandler(svc SandboxService) http.Handler {
	return NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{}, Auth: fakeAuth{}, Sandboxes: svc})
}

// D66: requests reach the sandbox's server under the path after the prefix,
// without the caller's credentials; responses come back without cookies and
// with a CSP sandbox header.
func TestPortProxy(t *testing.T) {
	var got *http.Request
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "evil", Path: "/ui"})
		io.WriteString(w, "hello from the app")
	}))
	defer app.Close()
	svc := &portSandboxes{addr: strings.TrimPrefix(app.URL, "http://")}
	h := portHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/ports/8080/api/items?x=1", strings.NewReader("body"))
	req.Header.Set("Authorization", "Bearer "+goodKey)
	req.Header.Set("Cookie", "session=mine")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 || rec.Body.String() != "hello from the app" {
		t.Fatalf("proxy = %d %q", rec.Code, rec.Body.String())
	}
	if got.Method != http.MethodPost || got.URL.Path != "/api/items" || got.URL.RawQuery != "x=1" {
		t.Fatalf("app saw %s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "" || got.Header.Get("Cookie") != "" {
		t.Fatal("the caller's API key or cookies reached the sandbox")
	}
	if got.Header.Get("X-Forwarded-Prefix") != "/v1/sandboxes/sb-1/ports/8080" {
		t.Fatalf("X-Forwarded-Prefix = %q", got.Header.Get("X-Forwarded-Prefix"))
	}
	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("response headers = %v", rec.Header())
	}
	if len(svc.opened) != 1 || svc.opened[0] != 8080 || svc.released != 1 {
		t.Fatalf("opened %v, released %d", svc.opened, svc.released)
	}
}

func TestPortProxyErrors(t *testing.T) {
	h := portHandler(&portSandboxes{})
	for _, c := range []struct {
		path string
		want int
	}{
		{"/v1/sandboxes/sb-1/ports/8080/", http.StatusBadGateway},       // nothing listening
		{"/v1/sandboxes/sb-1/ports/http/", http.StatusBadRequest},       // not a number
		{"/v1/sandboxes/sb-1/ports/70000/", http.StatusBadRequest},      // out of range
		{"/v1/sandboxes/sb-other/ports/8080/", http.StatusNotFound},     // not yours
		{"/v1/sandboxes/sb-1/ports/8080", http.StatusPermanentRedirect}, // add the slash
	} {
		rec := do(h, http.MethodGet, c.path, "")
		if rec.Code != c.want {
			t.Errorf("GET %s = %d (%s), want %d", c.path, rec.Code, rec.Body.String(), c.want)
		}
	}
	// Without a key, nothing is proxied.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/ports/8080/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", rec.Code)
	}
}
