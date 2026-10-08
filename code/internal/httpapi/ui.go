package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	gateway   string
	// Sandbox limits shown in and enforced by the create form.
	ttlChoices  []int
	idleTimeout time.Duration // the SSH gateway's public host:port, may be empty
	runtimes    []string      // offered in the create form, the default first
	pages       map[string]*template.Template
}

// page is what every template gets: the signed-in user (nil on the login
// page) plus page-specific fields.
type page struct {
	Title string
	User  *store.User
	Error string
	// Which optional sections exist (for the nav); set by render.
	HasKeys, HasAdmin bool
	// Self is the page's own URL, for its Refresh link (nothing refreshes
	// on its own, step 4.6).
	Self string
	// Pager is the main list's pagination (50 per page); UserPager is the
	// admin users table's.
	Pager, UserPager pager

	// Sandbox list and create form.
	Sandboxes  []store.Sandbox
	ShowEnded  bool
	Images     []string
	Runtimes   []string // the choice is shown only when there are several
	TTLChoices []int
	IdleFor    string // e.g. "30m"
	Form       createForm

	// ActiveKeys holds the signed-in user's active key IDs on pages showing
	// their own sandboxes, so "created by" can link to the key (or say it
	// was revoked); nil elsewhere.
	ActiveKeys map[string]bool

	// Sandbox detail.
	Sandbox    store.Sandbox
	SSHCommand string
	GatewaySet bool

	// API keys.
	Keys       []store.APIKey
	CurrentKey string // ID of the key this session uses
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
	Runtime      string
	SSHPublicKey string
	TTLHours     int
}

// ttlChoices are the lifetimes the create form can offer, in hours; only
// those within the configured maximum are shown (setLimits).
var ttlChoices = []int{1, 2, 4, 8, 24}

// setLimits applies the sandbox limits from config: the lifetimes offered
// (at most maxTTL; at least the shortest one) and the idle timeout stated
// in the form. Zero values keep the defaults.
func (u *ui) setLimits(maxTTL, idle time.Duration) {
	u.ttlChoices = ttlChoices
	if maxTTL > 0 {
		u.ttlChoices = nil
		for _, h := range ttlChoices {
			if time.Duration(h)*time.Hour <= maxTTL {
				u.ttlChoices = append(u.ttlChoices, h)
			}
		}
		if len(u.ttlChoices) == 0 {
			u.ttlChoices = ttlChoices[:1] // the service will explain the limit
		}
	}
	u.idleTimeout = 30 * time.Minute
	if idle > 0 {
		u.idleTimeout = idle
	}
}

// setRuntimes sets the runtimes the create form offers (D65).
func (u *ui) setRuntimes(runtimes []string) {
	u.runtimes = runtimes
	if len(u.runtimes) == 0 {
		u.runtimes = []string{"gvisor"}
	}
}

// templateFuncs are helpers available in every template.
var templateFuncs = template.FuncMap{
	"live":  func(status string) bool { return store.IsLive(status) },
	"ago":   func(t time.Time) string { return humanDuration(time.Since(t)) + " ago" },
	"until": until,
	"stamp": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	"key":   keyLabel,
	"keyref": func(k *store.KeyRef, active map[string]bool) keyRefData {
		return keyRefData{Ref: k, Active: active}
	},
}

// keyRefData is what the "keyref" template gets: the key and, on the
// owner's own pages, which of their keys are still active.
type keyRefData struct {
	Ref    *store.KeyRef
	Active map[string]bool
}

// keyLabel names the API key that created a sandbox: "ci-runner
// (sk_vOawHfdi…)", just the prefix for an unnamed key, or "—" for
// sandboxes created before keys were recorded (step 4.5).
func keyLabel(k *store.KeyRef) string {
	switch {
	case k == nil:
		return "—"
	case k.Name == "":
		return k.Prefix + "…"
	default:
		return k.Name + " (" + k.Prefix + "…)"
	}
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
	u.setLimits(0, 0)
	u.setRuntimes(nil)
	partials := []string{"templates/rows.html", "templates/status.html", "templates/admin_rows.html", "templates/keyref.html", "templates/pager.html"}
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
	mux.Handle("POST /ui/sandboxes", u.requireSession(http.HandlerFunc(u.create)))
	mux.Handle("GET /ui/sandboxes/{id}", u.requireSession(http.HandlerFunc(u.detail)))
	mux.Handle("POST /ui/sandboxes/{id}/delete", u.requireSession(http.HandlerFunc(u.delete)))
	// Keys and Admin are optional dependencies: without them those pages
	// don't exist (404) rather than crash.
	if u.keys != nil {
		mux.Handle("GET /ui/keys", u.requireSession(http.HandlerFunc(u.listKeys)))
		mux.Handle("POST /ui/keys", u.requireSession(http.HandlerFunc(u.createKey)))
		mux.Handle("POST /ui/keys/{id}/revoke", u.requireSession(http.HandlerFunc(u.revokeKey)))
	}
	if u.admin != nil {
		mux.Handle("GET /ui/admin", u.requireSession(requireAdmin(http.HandlerFunc(u.adminView))))
	}
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
		user, apiKey, err := u.auth.Authenticate(r.Context(), c.Value)
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
		next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), user, apiKey)))
	})
}

// render executes the page into a buffer first, so a template error becomes
// a clean 500 instead of a 200 with half a page (review of #35).
func (u *ui) render(w http.ResponseWriter, status int, name string, p page) {
	p.HasKeys, p.HasAdmin = u.keys != nil, u.admin != nil
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
	user, _, err := u.auth.Authenticate(r.Context(), key)
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

// toLogin sends the browser to the login page.
func toLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// listPage loads one page of the signed-in user's sandboxes for the list
// page (50 per page, step 4.6).
func (u *ui) listPage(r *http.Request) (page, error) {
	user := userFrom(r.Context())
	showEnded := r.URL.Query().Get("ended") == "1"
	n := requestedPage(r, "page")
	list, total, err := u.sandboxes.ListPage(r.Context(), user, showEnded, pageSize, (n-1)*pageSize)
	if err == nil && n > pages(total) { // past the end: show the last page
		n = clampPage(n, total)
		list, total, err = u.sandboxes.ListPage(r.Context(), user, showEnded, pageSize, (n-1)*pageSize)
	}
	if err != nil {
		return page{}, err
	}
	return page{ActiveKeys: u.activeKeys(r, user),
		Title: "Your sandboxes", User: &user, Pager: newPager(r, "page", n, total), Self: selfURL(r),
		Sandboxes: list, ShowEnded: showEnded,
		Images: sandbox.Images, Runtimes: u.runtimes, TTLChoices: u.ttlChoices, IdleFor: humanDuration(u.idleTimeout),
		Form: createForm{Image: sandbox.Images[0], Runtime: u.runtimes[0], TTLHours: 1},
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

func (u *ui) create(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	form := createForm{
		Image:        r.PostFormValue("image"),
		Runtime:      r.PostFormValue("runtime"),
		SSHPublicKey: strings.TrimSpace(r.PostFormValue("ssh_public_key")),
	}
	var err error
	form.TTLHours, err = strconv.Atoi(r.PostFormValue("ttl_hours"))
	var sb store.Sandbox
	if err != nil || !slices.Contains(u.ttlChoices, form.TTLHours) {
		// Only the offered lifetimes: a missing or bogus value must not
		// silently become the default (0 means "default" to the service).
		err = &sandbox.ValidationError{Msg: "pick a lifetime from the list"}
	} else {
		sb, err = u.sandboxes.Create(r.Context(), user, sandbox.CreateRequest{
			Image: form.Image, Runtime: form.Runtime, SSHPublicKey: form.SSHPublicKey, TTLSeconds: form.TTLHours * 3600,
			CreatedBy: callerKey(r.Context()),
		})
	}
	if err != nil {
		status, msg := serviceError(u.log, err)
		// The list as at GET /ui/: its Refresh and pager links come from the
		// request URL, and this one (POST /ui/sandboxes) can't be fetched.
		get := r.Clone(r.Context())
		get.URL = &url.URL{Path: "/ui/"}
		p, listErr := u.listPage(get)
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

// activeKeys is the set of the user's active key IDs, or nil when there's
// no keys page to link to. It's only for the "created by" link, so if the
// keys can't be loaded the page still renders, with a plain label.
func (u *ui) activeKeys(r *http.Request, user store.User) map[string]bool {
	if u.keys == nil {
		return nil
	}
	keys, err := u.keys.ListAPIKeys(r.Context(), user.ID)
	if err != nil {
		u.log.Warn("list api keys for created-by links", "err", err)
		return nil
	}
	active := make(map[string]bool, len(keys))
	for _, k := range keys {
		active[k.ID] = true
	}
	return active
}

// detailPage loads one sandbox the user may see (their own; any for an
// admin, as on /v1).
func (u *ui) detailPage(r *http.Request) (page, error) {
	user := userFrom(r.Context())
	sb, err := u.sandboxes.Get(r.Context(), user, r.PathValue("id"))
	if err != nil {
		return page{}, err
	}
	var active map[string]bool
	if sb.UserID == user.ID { // keys page links only make sense for your own
		active = u.activeKeys(r, user)
	}
	return page{ActiveKeys: active, Self: selfURL(r),
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

func (u *ui) delete(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	sb, err := u.sandboxes.Delete(r.Context(), user, r.PathValue("id"))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	back := "/ui/"
	// Back to the admin page when deleting from it (a fixed value, never a
	// URL from the form), or when an admin deleted someone else's sandbox.
	if r.PostFormValue("from") == "admin" || sb.UserID != user.ID {
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

// adminPage loads one page of users (?upage=) and one page of sandboxes
// (?page=) for the admin view (D43; 50 per page, step 4.6).
func (u *ui) adminPage(r *http.Request) (page, error) {
	user := userFrom(r.Context())
	p := page{Title: "Admin", User: &user, ShowEnded: r.URL.Query().Get("ended") == "1", Self: selfURL(r)}
	ctx := r.Context()
	n := requestedPage(r, "page")
	all, total, err := u.admin.ListAllSandboxes(ctx, p.ShowEnded, pageSize, (n-1)*pageSize)
	if err == nil && n > pages(total) {
		n = clampPage(n, total)
		all, total, err = u.admin.ListAllSandboxes(ctx, p.ShowEnded, pageSize, (n-1)*pageSize)
	}
	if err != nil {
		return p, err
	}
	p.AllSandboxes, p.Pager = all, newPager(r, "page", n, total)
	un := requestedPage(r, "upage")
	users, utotal, err := u.admin.ListUsers(ctx, pageSize, (un-1)*pageSize)
	if err == nil && un > pages(utotal) {
		un = clampPage(un, utotal)
		users, utotal, err = u.admin.ListUsers(ctx, pageSize, (un-1)*pageSize)
	}
	if err != nil {
		return p, err
	}
	p.Users, p.UserPager = users, newPager(r, "upage", un, utotal)
	return p, nil
}

func (u *ui) adminView(w http.ResponseWriter, r *http.Request) {
	p, err := u.adminPage(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, http.StatusOK, "admin", p)
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
// session uses.
func (u *ui) keysPage(r *http.Request) (page, error) {
	user := userFrom(r.Context())
	n := requestedPage(r, "page")
	keys, total, err := u.keys.ListAPIKeysPage(r.Context(), user.ID, pageSize, (n-1)*pageSize)
	if err == nil && n > pages(total) {
		n = clampPage(n, total)
		keys, total, err = u.keys.ListAPIKeysPage(r.Context(), user.ID, pageSize, (n-1)*pageSize)
	}
	p := page{Title: "API keys", User: &user, Keys: keys, Pager: newPager(r, "page", n, total), Self: selfURL(r)}
	if r.URL.Query().Get("error") == "last-key" { // a fixed code, never echoed text
		p.Error = "You can't revoke your only API key. Create another one first."
	}
	p.CurrentKey = keyFrom(r.Context()).ID
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

// fail shows a service error as a plain error page.
func (u *ui) fail(w http.ResponseWriter, _ *http.Request, err error) {
	status, msg := serviceError(u.log, err)
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
