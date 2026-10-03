package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/qfliu/sandbox-provider-temper/code/internal/auth"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
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
	log       *slog.Logger
	auth      Authenticator
	sandboxes SandboxService
	keys      KeyService
	admin     AdminService
	gateway   string // the SSH gateway's public host:port, may be empty
	pages     map[string]*template.Template
}

// page is what every template gets: the signed-in user (nil on the login
// page) plus page-specific fields.
type page struct {
	Title string
	User  *store.User
	Error string

	// Sandbox list and create form.
	Sandboxes  []store.Sandbox
	ShowEnded  bool
	Images     []string
	TTLChoices []int
	Form       createForm

	// Sandbox detail.
	Sandbox    store.Sandbox
	SSHCommand string
	GatewaySet bool

	// API keys.
	Keys       []store.APIKey
	CurrentKey string // display prefix of the key this session uses
	NewKey     string // a just-created key, shown once
	NewKeyName string

	// Admin view.
	Users        []store.UserSummary
	AllSandboxes []store.OwnedSandbox
}

// createForm keeps what the user typed when the form is shown again with
// an error.
type createForm struct {
	Image        string
	SSHPublicKey string
	TTLHours     int
}

// ttlChoices are the lifetimes offered in the create form, in hours. The
// service still enforces the configured maximum.
var ttlChoices = []int{1, 2, 4, 8, 24}

// templateFuncs are helpers available in every template.
var templateFuncs = template.FuncMap{
	"live":  func(status string) bool { return store.IsLive(status) },
	"ago":   func(t time.Time) string { return humanDuration(time.Since(t)) + " ago" },
	"until": until,
	"stamp": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
}

// until is "in 52m", or "expired" once t has passed (a live sandbox can be
// briefly past its expiry until the reaper's next run).
func until(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	return "in " + humanDuration(d)
}

// humanDuration rounds d for display: "under a minute", "5m", "2h 5m".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func newUI(log *slog.Logger, auth Authenticator, sandboxes SandboxService, keys KeyService, admin AdminService, gateway string) *ui {
	u := &ui{log: log, auth: auth, sandboxes: sandboxes, keys: keys, admin: admin, gateway: gateway, pages: map[string]*template.Template{}}
	partials := []string{"templates/rows.html", "templates/status.html", "templates/admin_rows.html"}
	for _, name := range []string{"login", "list", "detail", "keys", "admin"} {
		files := append([]string{"templates/layout.html", "templates/" + name + ".html"}, partials...)
		u.pages[name] = template.Must(template.New(name).Funcs(templateFuncs).ParseFS(web.Templates, files...))
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
	mux.Handle("GET /ui/{$}", u.requireSession(http.HandlerFunc(u.list)))
	mux.Handle("GET /ui/sandboxes/rows", u.requireSession(http.HandlerFunc(u.rows)))
	mux.Handle("POST /ui/sandboxes", u.requireSession(http.HandlerFunc(u.create)))
	mux.Handle("GET /ui/sandboxes/{id}", u.requireSession(http.HandlerFunc(u.detail)))
	mux.Handle("GET /ui/sandboxes/{id}/status", u.requireSession(http.HandlerFunc(u.status)))
	mux.Handle("POST /ui/sandboxes/{id}/delete", u.requireSession(http.HandlerFunc(u.delete)))
	mux.Handle("GET /ui/keys", u.requireSession(http.HandlerFunc(u.listKeys)))
	mux.Handle("POST /ui/keys", u.requireSession(http.HandlerFunc(u.createKey)))
	mux.Handle("POST /ui/keys/{id}/revoke", u.requireSession(http.HandlerFunc(u.revokeKey)))
	mux.Handle("GET /ui/admin", u.requireSession(requireAdmin(http.HandlerFunc(u.adminView))))
	mux.Handle("GET /ui/admin/rows", u.requireSession(requireAdmin(http.HandlerFunc(u.adminRows))))
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
			toLogin(w, r)
			return
		}
		user, err := u.auth.UserByAPIKey(r.Context(), c.Value)
		if errors.Is(err, store.ErrNotFound) { // revoked or bogus: end the session
			clearSession(w, r)
			toLogin(w, r)
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
	u.renderTemplate(w, status, name, "layout", p)
}

// renderTemplate renders one named template of a page; partials (htmx
// refreshes) use it directly.
func (u *ui) renderTemplate(w http.ResponseWriter, status int, name, tmpl string, p page) {
	var buf bytes.Buffer
	if err := u.pages[name].ExecuteTemplate(&buf, tmpl, p); err != nil {
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

// toLogin sends the browser to the login page. An htmx background request
// (auto-refresh) can't follow a plain redirect usefully, since htmx would
// paste the login page into the table, so it gets HX-Redirect instead,
// which makes htmx load the login page as a whole.
func toLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/ui/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// listPage loads the signed-in user's sandboxes for the list page.
func (u *ui) listPage(r *http.Request) (page, error) {
	user := userFrom(r.Context())
	showEnded := r.URL.Query().Get("ended") == "1"
	list, err := u.sandboxes.List(r.Context(), user, showEnded)
	return page{
		Title: "Your sandboxes", User: &user,
		Sandboxes: list, ShowEnded: showEnded,
		Images: sandbox.Images, TTLChoices: ttlChoices,
		Form: createForm{Image: sandbox.Images[0], TTLHours: 1},
	}, err
}

func (u *ui) list(w http.ResponseWriter, r *http.Request) {
	p, err := u.listPage(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, http.StatusOK, "list", p)
}

// rows is the list's table alone, for the htmx auto-refresh.
func (u *ui) rows(w http.ResponseWriter, r *http.Request) {
	p, err := u.listPage(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.renderTemplate(w, http.StatusOK, "list", "rows", p)
}

func (u *ui) create(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	form := createForm{
		Image:        r.PostFormValue("image"),
		SSHPublicKey: strings.TrimSpace(r.PostFormValue("ssh_public_key")),
	}
	var err error
	form.TTLHours, err = strconv.Atoi(r.PostFormValue("ttl_hours"))
	var sb store.Sandbox
	if err != nil || !slices.Contains(ttlChoices, form.TTLHours) {
		// Only the offered lifetimes: a missing or bogus value must not
		// silently become the default (0 means "default" to the service).
		err = &sandbox.ValidationError{Msg: "pick a lifetime from the list"}
	} else {
		sb, err = u.sandboxes.Create(r.Context(), user, sandbox.CreateRequest{
			Image: form.Image, SSHPublicKey: form.SSHPublicKey, TTLSeconds: form.TTLHours * 3600,
		})
	}
	if err != nil {
		status, msg := serviceError(u.log, err)
		p, listErr := u.listPage(r)
		if listErr != nil {
			u.fail(w, r, listErr)
			return
		}
		p.Error, p.Form = "Couldn't create the sandbox: "+msg+".", form
		u.render(w, status, "list", p)
		return
	}
	http.Redirect(w, r, "/ui/sandboxes/"+sb.ID, http.StatusSeeOther)
}

// detailPage loads one sandbox the user may see (their own; any for an
// admin, as on /v1).
func (u *ui) detailPage(r *http.Request) (page, error) {
	user := userFrom(r.Context())
	sb, err := u.sandboxes.Get(r.Context(), user, r.PathValue("id"))
	return page{
		Title: sb.ID, User: &user, Sandbox: sb,
		SSHCommand: sshCommand(sb.ID, u.gateway), GatewaySet: gatewaySet(u.gateway),
	}, err
}

func (u *ui) detail(w http.ResponseWriter, r *http.Request) {
	p, err := u.detailPage(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, http.StatusOK, "detail", p)
}

// status is the detail page's status card alone, for the htmx refresh.
func (u *ui) status(w http.ResponseWriter, r *http.Request) {
	p, err := u.detailPage(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.renderTemplate(w, http.StatusOK, "detail", "status", p)
}

func (u *ui) delete(w http.ResponseWriter, r *http.Request) {
	if _, err := u.sandboxes.Delete(r.Context(), userFrom(r.Context()), r.PathValue("id")); err != nil {
		u.fail(w, r, err)
		return
	}
	back := "/ui/"
	if r.PostFormValue("from") == "admin" { // a fixed value, never a URL from the form
		back = "/ui/admin"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// requireAdmin lets only admins through (after requireSession). Others get
// a 404, so the admin pages don't reveal that they exist.
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !userFrom(r.Context()).IsAdmin {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminPage loads every user and sandbox for the admin view (D43).
func (u *ui) adminPage(r *http.Request, withUsers bool) (page, error) {
	user := userFrom(r.Context())
	p := page{Title: "Admin", User: &user, ShowEnded: r.URL.Query().Get("ended") == "1"}
	var err error
	if p.AllSandboxes, err = u.admin.ListAllSandboxes(r.Context(), p.ShowEnded); err != nil {
		return p, err
	}
	if withUsers {
		p.Users, err = u.admin.ListUsers(r.Context())
	}
	return p, err
}

func (u *ui) adminView(w http.ResponseWriter, r *http.Request) {
	p, err := u.adminPage(r, true)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, http.StatusOK, "admin", p)
}

// adminRows is the admin sandbox table alone, for the htmx auto-refresh.
func (u *ui) adminRows(w http.ResponseWriter, r *http.Request) {
	p, err := u.adminPage(r, false)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.renderTemplate(w, http.StatusOK, "admin", "admin_rows", p)
}

// maxKeyNameLen bounds a key's label.
const maxKeyNameLen = 100

// checkKeyName returns why a key name is unacceptable, or "". The limit
// counts characters, like the form's maxlength; control characters (which
// Postgres may reject, e.g. NUL) and invalid UTF-8 are refused.
func checkKeyName(name string) string {
	if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "Name can't contain control characters."
	}
	if utf8.RuneCountInString(name) > maxKeyNameLen {
		return fmt.Sprintf("Name too long (at most %d characters).", maxKeyNameLen)
	}
	return ""
}

// keysPage loads the signed-in user's active keys and notes which one this
// session uses (by display prefix: 8 random characters, so two of a user's
// keys sharing one is vanishingly unlikely, and it's only used for display
// and the revoke warning).
func (u *ui) keysPage(r *http.Request) (page, error) {
	user := userFrom(r.Context())
	keys, err := u.keys.ListAPIKeys(r.Context(), user.ID)
	p := page{Title: "API keys", User: &user, Keys: keys}
	if r.URL.Query().Get("error") == "last-key" { // a fixed code, never echoed text
		p.Error = "You can't revoke your only API key. Create another one first."
	}
	if c, cerr := r.Cookie(sessionCookie); cerr == nil && len(c.Value) >= auth.DisplayPrefixLen {
		p.CurrentKey = c.Value[:auth.DisplayPrefixLen]
	}
	return p, err
}

func (u *ui) listKeys(w http.ResponseWriter, r *http.Request) {
	p, err := u.keysPage(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, http.StatusOK, "keys", p)
}

// createKey issues a key and shows it in this response only: it's never
// stored in plain text, and the page isn't cached (no-store).
func (u *ui) createKey(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	if msg := checkKeyName(name); msg != "" {
		p, err := u.keysPage(r)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		p.Error = msg
		u.render(w, http.StatusBadRequest, "keys", p)
		return
	}
	plaintext, key, err := u.keys.CreateAPIKey(r.Context(), user.ID, name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.log.Info("api key created", "user", user.Name, "key", key.Prefix)
	p, err := u.keysPage(r)
	if err != nil {
		// The key exists now: show it anyway, or nobody would ever see it.
		u.log.Error("list api keys after create", "err", err)
		user := userFrom(r.Context())
		p = page{Title: "API keys", User: &user, Error: "Couldn't load your other keys; reload the page to see them."}
	}
	p.NewKey, p.NewKeyName = plaintext, key.Name
	u.render(w, http.StatusOK, "keys", p)
}

func (u *ui) revokeKey(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	err := u.keys.RevokeAPIKey(r.Context(), user.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrLastKey) {
		// Redirect (not render) so a reload doesn't resend the POST.
		http.Redirect(w, r, "/ui/keys?error=last-key", http.StatusSeeOther)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "API key not found", http.StatusNotFound)
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.log.Info("api key revoked", "user", user.Name, "key_id", r.PathValue("id"))
	// If that was this session's key, the next page load signs the user out
	// (requireSession rejects revoked keys).
	http.Redirect(w, r, "/ui/keys", http.StatusSeeOther)
}

// fail shows a service error as a plain error page (or, for an htmx
// refresh, just the status, which leaves the current content in place).
func (u *ui) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := serviceError(u.log, err)
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(status)
		return
	}
	http.Error(w, msg, status)
}

// gatewaySet reports whether the gateway address is usable: not empty, not
// ":30022" (a failed `minikube ip`), and not the manifest's unreplaced
// placeholder.
func gatewaySet(gateway string) bool {
	host, _, err := net.SplitHostPort(gateway)
	if err != nil {
		host = gateway
	}
	return host != "" && host != "SSH_GATEWAY_ADDR"
}

// sshCommand is what a user types to reach the sandbox through the gateway
// (D6, D38): "ssh -p 30022 sb-…@192.168.64.2", or a placeholder host when
// the gateway's address isn't configured.
func sshCommand(id, gateway string) string {
	host, port, err := net.SplitHostPort(gateway)
	if err != nil {
		host, port = gateway, "22"
	}
	if !gatewaySet(gateway) {
		return "ssh " + id + "@<gateway>"
	}
	if port == "22" {
		return "ssh " + id + "@" + host
	}
	return "ssh -p " + port + " " + id + "@" + host
}
