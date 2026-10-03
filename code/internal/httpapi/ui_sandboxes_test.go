package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// ownedSandboxes is a sandbox service that, like the real one, only lets a
// user see and delete their own sandboxes (any, for an admin).
type ownedSandboxes struct {
	mu      sync.Mutex
	rows    map[string]store.Sandbox
	created []sandbox.CreateRequest
	deleted []string
}

func newOwnedSandboxes(rows ...store.Sandbox) *ownedSandboxes {
	f := &ownedSandboxes{rows: map[string]store.Sandbox{}}
	for _, r := range rows {
		f.rows[r.ID] = r
	}
	return f
}

func (f *ownedSandboxes) Create(_ context.Context, u store.User, req sandbox.CreateRequest) (store.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(req.SSHPublicKey, "ssh-") {
		return store.Sandbox{}, &sandbox.ValidationError{Msg: "ssh_public_key is not a valid public key"}
	}
	f.created = append(f.created, req)
	sb := store.Sandbox{ID: "sb-new000000001", UserID: u.ID, Image: req.Image, Status: store.StatusPending,
		ExpiresAt: time.Now().Add(time.Duration(req.TTLSeconds) * time.Second)}
	f.rows[sb.ID] = sb
	return sb, nil
}

func (f *ownedSandboxes) Get(_ context.Context, u store.User, id string) (store.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.rows[id]
	if !ok || (sb.UserID != u.ID && !u.IsAdmin) {
		return store.Sandbox{}, store.ErrNotFound
	}
	return sb, nil
}

func (f *ownedSandboxes) List(_ context.Context, u store.User, includeEnded bool) ([]store.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Sandbox
	for _, sb := range f.rows {
		if sb.UserID == u.ID && (includeEnded || store.IsLive(sb.Status)) {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (f *ownedSandboxes) Delete(ctx context.Context, u store.User, id string) (store.Sandbox, error) {
	sb, err := f.Get(ctx, u, id)
	if err != nil {
		return sb, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	sb.Status = store.StatusDeleted
	f.rows[id] = sb
	return sb, nil
}

func (f *ownedSandboxes) Exec(context.Context, store.User, string, sandbox.ExecRequest) (sshx.ExecResult, error) {
	return sshx.ExecResult{}, nil
}
func (f *ownedSandboxes) Upload(context.Context, store.User, string, string, io.Reader) (int64, error) {
	return 0, nil
}
func (f *ownedSandboxes) Download(context.Context, store.User, string, string, func(int64), io.Writer) error {
	return nil
}

// alice (u1, via goodKey) owns two sandboxes, one ended; bob (u9) owns one.
func aliceAndBob() *ownedSandboxes {
	now := time.Now()
	return newOwnedSandboxes(
		store.Sandbox{ID: "sb-alice0000001", UserID: "u1", Image: "ubuntu-dev", Status: store.StatusRunning,
			ExpiresAt: now.Add(50 * time.Minute), LastActivityAt: now.Add(-3 * time.Minute), CreatedAt: now.Add(-10 * time.Minute)},
		store.Sandbox{ID: "sb-alice0000old", UserID: "u1", Image: "ubuntu-base", Status: store.StatusDeleted,
			StatusReason: "deleted by owner", LastActivityAt: now.Add(-2 * time.Hour)},
		store.Sandbox{ID: "sb-bob000000001", UserID: "u9", Image: "ubuntu-base", Status: store.StatusRunning,
			ExpiresAt: now.Add(time.Hour)},
	)
}

func sandboxUI(svc SandboxService, gateway string) http.Handler {
	return NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{},
		Auth: uiAuth{}, Sandboxes: svc, SSHGateway: gateway})
}

var alice = &http.Cookie{Name: sessionCookie, Value: goodKey}

func TestUIListShowsOnlyOwnLiveSandboxes(t *testing.T) {
	h := sandboxUI(aliceAndBob(), "")
	body := uiDo(h, http.MethodGet, "/ui/", nil, alice).Body.String()
	if !strings.Contains(body, "sb-alice0000001") || !strings.Contains(body, `class="status s-running"`) {
		t.Fatalf("own running sandbox missing:\n%s", body)
	}
	if strings.Contains(body, "sb-alice0000old") {
		t.Fatal("ended sandbox listed without ?ended=1")
	}
	if strings.Contains(body, "sb-bob") {
		t.Fatal("another user's sandbox listed")
	}
	if !strings.Contains(body, `hx-get="/ui/sandboxes/rows"`) || !strings.Contains(body, `hx-trigger="every 5s"`) {
		t.Fatal("list doesn't auto-refresh")
	}
	ended := uiDo(h, http.MethodGet, "/ui/?ended=1", nil, alice).Body.String()
	if !strings.Contains(ended, "sb-alice0000old") || !strings.Contains(ended, `hx-get="/ui/sandboxes/rows?ended=1"`) {
		t.Fatal("?ended=1 doesn't show ended sandboxes or keep the filter on refresh")
	}
}

// The refresh endpoint returns just the table, not a whole page.
func TestUIRowsPartial(t *testing.T) {
	rec := uiDo(sandboxUI(aliceAndBob(), ""), http.MethodGet, "/ui/sandboxes/rows", nil, alice)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "sb-alice0000001") {
		t.Fatalf("rows = %d:\n%s", rec.Code, body)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "New sandbox") {
		t.Fatal("rows partial contains the whole page")
	}
}

func TestUICreateSandbox(t *testing.T) {
	svc := aliceAndBob()
	h := sandboxUI(svc, "")
	rec := uiDo(h, http.MethodPost, "/ui/sandboxes", url.Values{
		"image": {"ubuntu-dev"}, "ssh_public_key": {"  ssh-ed25519 AAAAkey me@mac \n"}, "ttl_hours": {"4"},
	}, alice)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/sandboxes/sb-new000000001" {
		t.Fatalf("create = %d → %q, want 303 → the new sandbox", rec.Code, rec.Header().Get("Location"))
	}
	got := svc.created[0]
	if got.Image != "ubuntu-dev" || got.SSHPublicKey != "ssh-ed25519 AAAAkey me@mac" || got.TTLSeconds != 4*3600 {
		t.Fatalf("create request = %+v", got)
	}
}

// A rejected create shows the service's message and keeps what was typed.
func TestUICreateSandboxError(t *testing.T) {
	rec := uiDo(sandboxUI(aliceAndBob(), ""), http.MethodPost, "/ui/sandboxes", url.Values{
		"image": {"ubuntu-dev"}, "ssh_public_key": {"not-a-key"}, "ttl_hours": {"8"},
	}, alice)
	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest || !strings.Contains(body, "ssh_public_key is not a valid public key") {
		t.Fatalf("= %d, want 400 with the validation message:\n%s", rec.Code, body)
	}
	if !strings.Contains(body, ">not-a-key</textarea>") || !strings.Contains(body, `<option value="8" selected>`) ||
		!strings.Contains(body, `<option value="ubuntu-dev" selected>`) {
		t.Fatal("form values not kept after an error")
	}
}

func TestUIDetailPage(t *testing.T) {
	h := sandboxUI(aliceAndBob(), "192.168.64.2:30022")
	body := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-alice0000001", nil, alice).Body.String()
	for _, want := range []string{
		"ssh -p 30022 sb-alice0000001@192.168.64.2", // copyable ssh command
		`data-copy="ssh-cmd"`,
		"ubuntu-dev", "in 50m", "3m ago",
		`action="/ui/sandboxes/sb-alice0000001/delete"`, `data-confirm=`,
		`hx-get="/ui/sandboxes/sb-alice0000001/status"`, // live: status refreshes
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
	ended := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-alice0000old", nil, alice).Body.String()
	if strings.Contains(ended, "/delete") || strings.Contains(ended, "hx-trigger") {
		t.Error("an ended sandbox still offers delete or keeps refreshing")
	}
	if !strings.Contains(ended, "deleted by owner") {
		t.Error("status reason not shown")
	}
}

// Another user's sandbox looks like it doesn't exist, on every route.
func TestUIOtherUsersSandboxIsNotFound(t *testing.T) {
	svc := aliceAndBob()
	h := sandboxUI(svc, "")
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/ui/sandboxes/sb-bob000000001"},
		{http.MethodGet, "/ui/sandboxes/sb-bob000000001/status"},
		{http.MethodPost, "/ui/sandboxes/sb-bob000000001/delete"},
	} {
		var form url.Values
		if r.method == http.MethodPost {
			form = url.Values{}
		}
		if rec := uiDo(h, r.method, r.path, form, alice); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", r.method, r.path, rec.Code)
		}
	}
	if len(svc.deleted) != 0 {
		t.Fatalf("deleted %v", svc.deleted)
	}
}

func TestUIDeleteSandbox(t *testing.T) {
	svc := aliceAndBob()
	rec := uiDo(sandboxUI(svc, ""), http.MethodPost, "/ui/sandboxes/sb-alice0000001/delete", url.Values{}, alice)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("delete = %d → %q, want 303 → /ui/", rec.Code, rec.Header().Get("Location"))
	}
	if len(svc.deleted) != 1 || svc.deleted[0] != "sb-alice0000001" {
		t.Fatalf("deleted = %v", svc.deleted)
	}
}

// Deleting via GET (a link, a prefetch) must not be possible.
func TestUIDeleteNeedsPOST(t *testing.T) {
	svc := aliceAndBob()
	rec := uiDo(sandboxUI(svc, ""), http.MethodGet, "/ui/sandboxes/sb-alice0000001/delete", nil, alice)
	if rec.Code == http.StatusSeeOther || len(svc.deleted) != 0 {
		t.Fatalf("GET delete = %d, deleted %v", rec.Code, svc.deleted)
	}
}

// When the session is gone, an htmx auto-refresh gets HX-Redirect (so the
// whole page goes to login) instead of a redirect whose login page htmx
// would paste into the table.
func TestUIHtmxRefreshWithoutSession(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ui/sandboxes/rows", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	sandboxUI(aliceAndBob(), "").ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != "/ui/login" {
		t.Fatalf("= %d HX-Redirect %q, want 401 + HX-Redirect /ui/login", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}

func TestSSHCommand(t *testing.T) {
	for gateway, want := range map[string]string{
		"192.168.64.2:30022": "ssh -p 30022 sb-x@192.168.64.2",
		"gw.example.com:22":  "ssh sb-x@gw.example.com",
		"gw.example.com":     "ssh sb-x@gw.example.com",
		"":                   "ssh sb-x@<gateway>",
		":30022":             "ssh sb-x@<gateway>", // `minikube ip` failed
		"SSH_GATEWAY_ADDR":   "ssh sb-x@<gateway>", // manifest applied without deploy-api.sh
	} {
		if got := sshCommand("sb-x", gateway); got != want {
			t.Errorf("sshCommand(%q) = %q, want %q", gateway, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second:                "under a minute",
		5 * time.Minute:                 "5m",
		2*time.Hour + 5*time.Minute:     "2h 5m",
		3 * time.Hour:                   "3h",
		59*time.Minute + 50*time.Second: "1h",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

// htmx and the dashboard script are served from our own origin (the CSP
// allows nothing else).
func TestUIServesScripts(t *testing.T) {
	h := sandboxUI(aliceAndBob(), "")
	for _, f := range []string{"htmx.min.js", "app.js"} {
		rec := uiDo(h, http.MethodGet, "/ui/static/"+f, nil, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
			t.Errorf("%s = %d %q", f, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

// Only the offered lifetimes are accepted: a missing or bogus value must
// not silently become the 1 h default.
func TestUICreateRejectsBadLifetime(t *testing.T) {
	svc := aliceAndBob()
	h := sandboxUI(svc, "")
	for _, ttl := range []string{"", "abc", "3", "-1", "1000"} {
		rec := uiDo(h, http.MethodPost, "/ui/sandboxes", url.Values{
			"image": {"ubuntu-dev"}, "ssh_public_key": {"ssh-ed25519 AAAAkey"}, "ttl_hours": {ttl},
		}, alice)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "pick a lifetime") {
			t.Errorf("ttl_hours=%q: %d, want 400 asking for a lifetime", ttl, rec.Code)
		}
	}
	if len(svc.created) != 0 {
		t.Fatalf("created %v", svc.created)
	}
}

func TestUntil(t *testing.T) {
	if got := until(time.Now().Add(-90 * time.Minute)); got != "expired" {
		t.Errorf("until(past) = %q, want expired", got)
	}
	if got := until(time.Now().Add(50*time.Minute + 10*time.Second)); got != "in 50m" {
		t.Errorf("until(+50m) = %q", got)
	}
}

// The status card (the htmx refresh) carries the Delete button and its own
// polling, so both disappear in place once the sandbox ends.
func TestUIStatusPartial(t *testing.T) {
	h := sandboxUI(aliceAndBob(), "")
	live := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-alice0000001/status", nil, alice).Body.String()
	if !strings.Contains(live, `hx-get="/ui/sandboxes/sb-alice0000001/status"`) || !strings.Contains(live, "/delete") {
		t.Errorf("live status partial lacks polling or delete:\n%s", live)
	}
	if strings.Contains(live, "<html") {
		t.Error("status partial is a whole page")
	}
	ended := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-alice0000old/status", nil, alice).Body.String()
	if strings.Contains(ended, "hx-get") || strings.Contains(ended, "/delete") {
		t.Errorf("ended status partial still polls or offers delete:\n%s", ended)
	}
}

// The new state-changing routes are covered by the CSRF protection too.
func TestUISandboxRoutesRefuseCrossSitePOSTs(t *testing.T) {
	svc := aliceAndBob()
	h := sandboxUI(svc, "")
	for _, path := range []string{"/ui/sandboxes", "/ui/sandboxes/sb-alice0000001/delete"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("image=ubuntu-dev&ssh_public_key=ssh-ed25519+AAAA&ttl_hours=1"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(alice)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s = %d, want 403", path, rec.Code)
		}
	}
	if len(svc.created) != 0 || len(svc.deleted) != 0 {
		t.Fatalf("cross-site POSTs changed state: created %v, deleted %v", svc.created, svc.deleted)
	}
}

// The create form follows the configured limits: only lifetimes up to
// MaxTTL are offered or accepted, and the idle hint states IdleTimeout.
func TestUICreateFormFollowsConfig(t *testing.T) {
	svc := aliceAndBob()
	h := NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{}, Auth: uiAuth{},
		Sandboxes: svc, MaxTTL: 8 * time.Hour, IdleTimeout: 10 * time.Minute})
	body := uiDo(h, http.MethodGet, "/ui/", nil, alice).Body.String()
	if !strings.Contains(body, `<option value="8"`) || strings.Contains(body, `<option value="24"`) {
		t.Error("lifetimes not limited to MaxTTL 8h")
	}
	if !strings.Contains(body, "after 10m without use") {
		t.Error("idle hint doesn't state the configured timeout")
	}
	rec := uiDo(h, http.MethodPost, "/ui/sandboxes", url.Values{
		"image": {"ubuntu-dev"}, "ssh_public_key": {"ssh-ed25519 AAAAkey"}, "ttl_hours": {"24"},
	}, alice)
	if rec.Code != http.StatusBadRequest || len(svc.created) != 0 {
		t.Fatalf("24h with MaxTTL 8h = %d (created %v), want 400", rec.Code, svc.created)
	}
	// Defaults when unset.
	def := uiDo(sandboxUI(aliceAndBob(), ""), http.MethodGet, "/ui/", nil, alice).Body.String()
	if !strings.Contains(def, `<option value="24"`) || !strings.Contains(def, "after 30m without use") {
		t.Error("defaults not applied when limits are unset")
	}
}

// Creating a sandbox records the key that made the request, from /v1 and
// from the dashboard, and a client can't claim another key in the body.
func TestCreateRecordsKey(t *testing.T) {
	svc := aliceAndBob()
	h := sandboxUI(svc, "")
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes",
		strings.NewReader(`{"image":"ubuntu-dev","ssh_public_key":"ssh-ed25519 AAAAkey"}`))
	req.Header.Set("Authorization", "Bearer "+goodKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("/v1 create = %d %s", rec.Code, rec.Body)
	}
	if got := svc.created[0].CreatedBy; got == nil || got.ID != "k-good" || got.Name != "laptop" {
		t.Fatalf("/v1 create recorded %+v, want key k-good", got)
	}

	spoof := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(
		`{"image":"ubuntu-dev","ssh_public_key":"ssh-ed25519 AAAAkey","created_by":{"id":"k-other"},"CreatedBy":{"id":"k-other"}}`))
	spoof.Header.Set("Authorization", "Bearer "+goodKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, spoof)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a created_by field = %d, want 400", rec.Code)
	}

	uiDo(h, http.MethodPost, "/ui/sandboxes", url.Values{
		"image": {"ubuntu-dev"}, "ssh_public_key": {"ssh-ed25519 AAAAkey"}, "ttl_hours": {"1"},
	}, aliceSession)
	if got := svc.created[len(svc.created)-1].CreatedBy; got == nil || got.ID != "k-session" {
		t.Fatalf("dashboard create recorded %+v, want the session key k-session", got)
	}
}

func TestUIShowsCreatedByKey(t *testing.T) {
	sb := aliceAndBob()
	sb.rows["sb-alice0000001"] = func() store.Sandbox {
		s := sb.rows["sb-alice0000001"]
		s.CreatedByKey = &store.KeyRef{ID: "k1", Name: "ci-runner", Prefix: "sk_vOawHfdi"}
		return s
	}()
	h := adminUI(sb)
	for _, path := range []string{"/ui/", "/ui/sandboxes/sb-alice0000001"} {
		if body := uiDo(h, http.MethodGet, path, nil, alice).Body.String(); !strings.Contains(body, "ci-runner (sk_vOawHfdi…)") {
			t.Errorf("%s doesn't show the creating key", path)
		}
	}
	if body := uiDo(h, http.MethodGet, "/ui/admin", nil, root).Body.String(); !strings.Contains(body, "alice <span class=\"muted\">· ci-runner (sk_vOawHfdi…)") {
		t.Error("admin view doesn't show owner · key")
	}
	// Sandboxes created before keys were recorded.
	if body := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-bob000000001", nil, root).Body.String(); !strings.Contains(body, "<dt>Created by</dt><dd>—</dd>") {
		t.Error("missing key not shown as —")
	}
	if got := keyLabel(&store.KeyRef{Prefix: "sk_abcdefgh"}); got != "sk_abcdefgh…" {
		t.Errorf("unnamed key label = %q", got)
	}
}
