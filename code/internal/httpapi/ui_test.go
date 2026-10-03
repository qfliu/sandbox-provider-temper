package httpapi

import (
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// uiAuth knows goodKey (alice), an admin key, and a key whose user name
// contains HTML; anything else is unknown.
type uiAuth struct{}

func (uiAuth) UserByAPIKey(ctx context.Context, key string) (store.User, error) {
	switch key {
	case "sk_admin":
		return store.User{ID: "u2", Name: "root", IsAdmin: true}, nil
	case "sk_html":
		return store.User{ID: "u3", Name: `<script>alert(1)</script>`}, nil
	}
	return fakeAuth{}.UserByAPIKey(ctx, key)
}

func uiHandler() http.Handler {
	return NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{}, Auth: uiAuth{}, Sandboxes: fakeSandboxes{}})
}

// uiDo sends a request like a same-origin browser would.
func uiDo(h http.Handler, method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie set (status %d)", sessionCookie, rec.Code)
	return nil
}

func TestUIRedirectsToLoginWithoutSession(t *testing.T) {
	h := uiHandler()
	for _, path := range []string{"/ui", "/ui/"} {
		rec := uiDo(h, http.MethodGet, path, nil, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET %s = %d, want 303", path, rec.Code)
		}
	}
	if loc := uiDo(h, http.MethodGet, "/ui/", nil, nil).Header().Get("Location"); loc != "/ui/login" {
		t.Fatalf("Location = %q, want /ui/login", loc)
	}
	if rec := uiDo(h, http.MethodGet, "/ui/login", nil, nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `name="api_key"`) {
		t.Fatalf("login page: %d %s", rec.Code, rec.Body)
	}
}

func TestUILoginSetsSafeSessionCookie(t *testing.T) {
	h := uiHandler()
	rec := uiDo(h, http.MethodPost, "/ui/login", url.Values{"api_key": {goodKey}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("login = %d → %q, want 303 → /ui/", rec.Code, rec.Header().Get("Location"))
	}
	c := sessionFrom(t, rec)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/ui" || c.MaxAge <= 0 {
		t.Fatalf("cookie = %+v; want HttpOnly, SameSite=Strict, Path=/ui, MaxAge > 0", c)
	}
	if c.Secure {
		t.Fatal("Secure set over plain http; browsers may not send it back on http://localhost")
	}

	home := uiDo(h, http.MethodGet, "/ui/", nil, c)
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "alice") {
		t.Fatalf("home with session = %d, want 200 showing alice", home.Code)
	}
}

func TestUILoginSecureCookieBehindHTTPS(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader("api_key="+goodKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	uiHandler().ServeHTTP(rec, req)
	if c := sessionFrom(t, rec); !c.Secure {
		t.Fatal("cookie not Secure behind an HTTPS proxy")
	}
}

func TestUILoginRejectsBadKeys(t *testing.T) {
	h := uiHandler()
	for _, key := range []string{"", "sk_nope", "   "} {
		rec := uiDo(h, http.MethodPost, "/ui/login", url.Values{"api_key": {key}}, nil)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Invalid or revoked API key") {
			t.Errorf("key %q: %d, want 401 with an error message", key, rec.Code)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookie && c.MaxAge > 0 {
				t.Errorf("key %q: session cookie set", key)
			}
		}
	}
	if rec := uiDo(h, http.MethodPost, "/ui/login", url.Values{"api_key": {"sk_boom"}}, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("DB error: %d, want 500", rec.Code)
	}
}

// A cookie whose key was revoked (or never existed) ends the session: the
// browser is sent to the login page and the cookie is cleared.
func TestUIRevokedKeyEndsSession(t *testing.T) {
	rec := uiDo(uiHandler(), http.MethodGet, "/ui/", nil, &http.Cookie{Name: sessionCookie, Value: "sk_revoked"})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("= %d → %q, want 303 → /ui/login", rec.Code, rec.Header().Get("Location"))
	}
	if c := sessionFrom(t, rec); c.MaxAge >= 0 {
		t.Fatalf("cookie not cleared: %+v", c)
	}
}

func TestUILogoutClearsSession(t *testing.T) {
	rec := uiDo(uiHandler(), http.MethodPost, "/ui/logout", url.Values{}, &http.Cookie{Name: sessionCookie, Value: goodKey})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", rec.Code)
	}
	if c := sessionFrom(t, rec); c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("cookie not cleared: %+v", c)
	}
}

// A page on another site must not be able to make the browser sign in or
// out (CSRF): cross-site POSTs are refused before any handler runs.
func TestUIRefusesCrossSitePOSTs(t *testing.T) {
	h := uiHandler()
	for _, path := range []string{"/ui/login", "/ui/logout"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("api_key="+goodKey))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: goodKey})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s = %d, want 403", path, rec.Code)
		}
		if len(rec.Result().Cookies()) > 0 {
			t.Errorf("cross-site POST %s changed cookies", path)
		}
	}
}

func TestUISecurityHeaders(t *testing.T) {
	rec := uiDo(uiHandler(), http.MethodGet, "/ui/login", nil, nil)
	for header, want := range map[string]string{
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
}

// Template output is HTML-escaped: a user name can't inject markup.
func TestUIEscapesUserData(t *testing.T) {
	rec := uiDo(uiHandler(), http.MethodGet, "/ui/", nil, &http.Cookie{Name: sessionCookie, Value: "sk_html"})
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("user name not escaped:\n%s", body)
	}
}

func TestUIShowsAdminBadge(t *testing.T) {
	rec := uiDo(uiHandler(), http.MethodGet, "/ui/", nil, &http.Cookie{Name: sessionCookie, Value: "sk_admin"})
	if !strings.Contains(rec.Body.String(), `class="badge">admin`) {
		t.Fatal("admin badge missing for an admin")
	}
	rec = uiDo(uiHandler(), http.MethodGet, "/ui/", nil, &http.Cookie{Name: sessionCookie, Value: goodKey})
	if strings.Contains(rec.Body.String(), `class="badge">admin`) {
		t.Fatal("admin badge shown to a non-admin")
	}
}

func TestUIServesStaticCSS(t *testing.T) {
	rec := uiDo(uiHandler(), http.MethodGet, "/ui/static/app.css", nil, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("app.css = %d %q, want 200 text/css", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Cache-Control") == "no-store" {
		t.Fatal("static files shouldn't be no-store")
	}
}

// Same-site pages and old browsers that only send Origin are covered too,
// not just Sec-Fetch-Site: cross-site (review of #35).
func TestUIRefusesOtherCrossOriginPOSTs(t *testing.T) {
	h := uiHandler()
	for name, set := range map[string]func(*http.Request){
		"same-site":       func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"origin mismatch": func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
	} {
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader("api_key="+goodKey))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		set(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, rec.Code)
		}
	}
}

// Error responses carry the security headers too.
func TestUIErrorResponsesHaveSecurityHeaders(t *testing.T) {
	h := uiHandler()
	cross := httptest.NewRequest(http.MethodPost, "/ui/logout", nil)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	for name, req := range map[string]*http.Request{
		"404":        httptest.NewRequest(http.MethodGet, "/ui/nope", nil),
		"403":        cross,
		"static dir": httptest.NewRequest(http.MethodGet, "/ui/static/", nil),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("%s: %d, want an error", name, rec.Code)
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: security headers missing", name)
		}
	}
}

// /ui/static/ serves files, not a listing of them.
func TestUIStaticHasNoDirectoryListing(t *testing.T) {
	rec := uiDo(uiHandler(), http.MethodGet, "/ui/static/", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /ui/static/ = %d, want 404", rec.Code)
	}
}

// The session cookie only works for /ui: /v1 still needs the
// Authorization header.
func TestV1IgnoresSessionCookie(t *testing.T) {
	rec := uiDo(uiHandler(), http.MethodGet, "/v1/me", nil, &http.Cookie{Name: sessionCookie, Value: goodKey})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/me with only the cookie = %d, want 401", rec.Code)
	}
}

// A template error is a clean 500, not a 200 with half a page.
func TestUITemplateErrorIs500(t *testing.T) {
	u := newUI(slog.New(slog.NewTextHandler(io.Discard, nil)), uiAuth{}, fakeSandboxes{}, "")
	u.pages["broken"] = template.Must(template.New("layout").Parse(`partial {{.Missing.Field}}`))
	rec := httptest.NewRecorder()
	u.render(rec, http.StatusOK, "broken", page{})
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("= %d %q, want a clean 500", rec.Code, rec.Body)
	}
}
