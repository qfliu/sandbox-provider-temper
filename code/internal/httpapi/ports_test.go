package httpapi

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"

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

// Review of #77: what a sandbox's response may not carry through this
// origin, 1xx responses dropped, redirects kept under the prefix, escaped
// paths kept escaped, and only canonical port numbers.
func TestPortProxyHardening(t *testing.T) {
	var gotRawPath string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		w.Header().Set("Set-Cookie", "x=1; Path=/ui")
		w.WriteHeader(http.StatusEarlyHints) // a 103 with a cookie
		for _, h := range []string{"Clear-Site-Data", "Strict-Transport-Security", "Alt-Svc", "WWW-Authenticate", "Access-Control-Allow-Origin"} {
			w.Header().Set(h, "x")
		}
		w.Header().Set("Location", "/login")
		w.WriteHeader(http.StatusFound)
	}))
	defer app.Close()
	h := portHandler(&portSandboxes{addr: strings.TrimPrefix(app.URL, "http://")})

	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sandboxes/sb-1/ports/8080/a%2Fb/c", nil)
	req.Header.Set("Authorization", "Bearer "+goodKey)
	var early []string
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error { early = append(early, strconv.Itoa(code)); return nil },
	}))
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(early) != 0 {
		t.Errorf("1xx responses passed on: %v", early)
	}
	for _, h := range []string{"Set-Cookie", "Clear-Site-Data", "Strict-Transport-Security", "Alt-Svc", "WWW-Authenticate", "Access-Control-Allow-Origin"} {
		if resp.Header.Get(h) != "" {
			t.Errorf("%s passed through", h)
		}
	}
	if got := resp.Header.Get("Location"); got != "/v1/sandboxes/sb-1/ports/8080/login" {
		t.Errorf("Location = %q, want it under the prefix", got)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Sandbox-Port") != "8080" {
		t.Errorf("headers = %v", resp.Header)
	}
	if gotRawPath != "/a%2Fb/c" {
		t.Errorf("app saw %q, want the escaped slash kept", gotRawPath)
	}

	for _, p := range []string{"+80", "0080"} {
		if rec := do(h, http.MethodGet, "/v1/sandboxes/sb-1/ports/"+p+"/", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("port %q = %d, want 400", p, rec.Code)
		}
	}
}

// Upgrades (WebSockets) pass through the proxy, wrapper included.
func TestPortProxyUpgrade(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "want an upgrade", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo: " + line)
		rw.Flush()
	}))
	defer app.Close()
	srv := httptest.NewServer(portHandler(&portSandboxes{addr: strings.TrimPrefix(app.URL, "http://")}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /v1/sandboxes/sb-1/ports/8080/ws HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+goodKey+
		"\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade through the proxy: %v %v", resp, err)
	}
	// Then the connection is the app's: a line goes there and back.
	io.WriteString(conn, "hello\n")
	if line, _ := br.ReadString('\n'); line != "echo: hello\n" {
		t.Fatalf("after the upgrade: %q", line)
	}
}
