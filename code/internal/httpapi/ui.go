package httpapi

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
	"github.com/qfliu/sandbox-provider-temper/code/web"
)

// The dashboard (D13, D40) lives under /ui. A user signs in with one of their
// API keys; the key is then kept in a cookie and checked on every request
// exactly like the Authorization header on /v1, so revoking the key ends the
// session too.
const (
	sessionCookie = "sb_session"
	sessionMaxAge = 12 * time.Hour
)

// ui renders the dashboard's pages.
type ui struct {
	log   *slog.Logger
	auth  Authenticator
	pages map[string]*template.Template
}

// page is what every template gets: the signed-in user (nil on the login
// page) plus page-specific fields.
type page struct {
	Title string
	User  *store.User
	Error string
}

func newUI(log *slog.Logger, auth Authenticator) *ui {
	u := &ui{log: log, auth: auth, pages: map[string]*template.Template{}}
	for _, name := range []string{"login", "home"} {
		u.pages[name] = template.Must(template.ParseFS(web.Templates,
			"templates/layout.html", "templates/"+name+".html"))
	}
	return u
}

// routes returns the /ui handler.
//
// Protections, all for a browser holding a session cookie:
//   - the cookie is HttpOnly (scripts can't read it), SameSite=Strict (other
//     sites' pages don't send it) and scoped to /ui;
//   - http.CrossOriginProtection rejects cross-site POSTs (CSRF);
//   - every response forbids framing (clickjacking) and caching, and the
//     CSP only allows our own scripts and styles.
func (u *ui) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(web.Static, "static")
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", noListing(http.FileServerFS(static))))
	mux.HandleFunc("GET /ui/login", u.loginForm)
	mux.HandleFunc("POST /ui/login", u.login)
	mux.HandleFunc("POST /ui/logout", u.logout)
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
	})
	mux.Handle("GET /ui/{$}", u.requireSession(http.HandlerFunc(u.home)))
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

// noListing serves files but not directory listings.
func noListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/ui/static/") {
			h.Set("Cache-Control", "no-store") // pages show account data
		}
		next.ServeHTTP(w, r)
	})
}

// requireSession lets a request through only with a valid session cookie,
// putting the user in the context like requireAuth does for /v1. Without
// one, the browser is sent to the login page.
func (u *ui) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		user, err := u.auth.UserByAPIKey(r.Context(), c.Value)
		if errors.Is(err, store.ErrNotFound) { // revoked or bogus: end the session
			clearSession(w, r)
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		if err != nil {
			u.log.Error("api key lookup failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}

// render executes the page into a buffer first, so a template error becomes
// a clean 500 instead of a 200 with half a page (review of #35).
func (u *ui) render(w http.ResponseWriter, status int, name string, p page) {
	var buf bytes.Buffer
	if err := u.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		u.log.Error("render page", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (u *ui) loginForm(w http.ResponseWriter, _ *http.Request) {
	u.render(w, http.StatusOK, "login", page{Title: "Sign in"})
}

func (u *ui) login(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PostFormValue("api_key"))
	user, err := u.auth.UserByAPIKey(r.Context(), key)
	if errors.Is(err, store.ErrNotFound) || (err == nil && key == "") {
		u.render(w, http.StatusUnauthorized, "login", page{Title: "Sign in", Error: "Invalid or revoked API key."})
		return
	}
	if err != nil {
		u.log.Error("api key lookup failed", "err", err)
		u.render(w, http.StatusInternalServerError, "login", page{Title: "Sign in", Error: "Something went wrong; try again."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: key, Path: "/ui",
		MaxAge: int(sessionMaxAge.Seconds()), HttpOnly: true,
		Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
	u.log.Info("dashboard sign-in", "user", user.Name)
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (u *ui) logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w, r)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/ui", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
}

// isHTTPS reports whether the browser reached us over HTTPS, directly or
// through a TLS-terminating proxy, so the cookie gets the Secure flag
// there. (Local dev is plain http://localhost, where Secure would stop the
// browser from sending it back in some browsers.)
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (u *ui) home(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	u.render(w, http.StatusOK, "home", page{Title: "Your sandboxes", User: &user})
}
