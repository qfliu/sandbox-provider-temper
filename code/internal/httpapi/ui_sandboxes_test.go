package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
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

// ListPage is List, newest first, sliced like the store's LIMIT/OFFSET.
func (f *ownedSandboxes) ListPage(ctx context.Context, u store.User, includeEnded bool, limit, offset int) ([]store.Sandbox, int, error) {
	all, _ := f.List(ctx, u, includeEnded)
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})
	return pageOf(all, limit, offset), len(all), nil
}

// pageOf slices list like LIMIT/OFFSET.
func pageOf[T any](list []T, limit, offset int) []T {
	if offset >= len(list) {
		return []T{}
	}
	return list[offset:min(len(list), offset+limit)]
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
	// Nothing refreshes on its own (step 4.6): a Refresh link instead.
	if strings.Contains(body, "hx-") || strings.Contains(body, "htmx") || !strings.Contains(body, `<a href="/ui/">Refresh</a>`) {
		t.Fatal("list should have a Refresh link and no auto-refresh")
	}
	ended := uiDo(h, http.MethodGet, "/ui/?ended=1", nil, alice).Body.String()
	if !strings.Contains(ended, "sb-alice0000old") || !strings.Contains(ended, `<a href="/ui/?ended=1">Refresh</a>`) {
		t.Fatal("?ended=1 doesn't show ended sandboxes or keep the filter on Refresh")
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

// The error page after a failed create is the list as at /ui/: its links
// must not point at POST-only /ui/sandboxes (review of #56).
func TestUICreateErrorPageLinks(t *testing.T) {
	body := uiDo(sandboxUI(manyFor(60), ""), http.MethodPost, "/ui/sandboxes", url.Values{
		"image": {"ubuntu-dev"}, "ssh_public_key": {"not-a-key"}, "ttl_hours": {"8"},
	}, alice).Body.String()
	if !strings.Contains(body, `<a href="/ui/">Refresh</a>`) || !strings.Contains(body, `href="/ui/?page=2"`) {
		t.Fatal("Refresh/pager links don't point at /ui/")
	}
	if strings.Contains(body, `href="/ui/sandboxes?`) || strings.Contains(body, `href="/ui/sandboxes"`) {
		t.Fatal("a link points at POST-only /ui/sandboxes")
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
		`<a href="/ui/sandboxes/sb-alice0000001">Refresh</a>`, // refreshed by hand
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

func TestSSHCommand(t *testing.T) {
	for gateway, want := range map[string]string{
		"192.168.64.2:30022": "ssh -p 30022 sb-x@192.168.64.2",
		"gw.example.com:22":  "ssh sb-x@gw.example.com",
		"gw.example.com":     "ssh sb-x@gw.example.com",
		"":                   "ssh sb-x@<gateway>",
		":30022":             "ssh sb-x@<gateway>", // `minikube ip` failed
		"SSH_GATEWAY_ADDR":   "ssh sb-x@<gateway>", // chart installed without api.sshGatewayAddr
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

// The runtime choice (D65) appears only when the cluster offers more than
// one, and what the user picks reaches the service.
func TestUIRuntimeChoice(t *testing.T) {
	one := uiDo(sandboxUI(aliceAndBob(), ""), http.MethodGet, "/ui/", nil, alice).Body.String()
	if strings.Contains(one, `name="runtime"`) {
		t.Error("runtime choice shown with only gVisor available")
	}
	svc := aliceAndBob()
	h := NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{}, Auth: uiAuth{},
		Sandboxes: svc, Runtimes: []string{"gvisor", "kata"}})
	both := uiDo(h, http.MethodGet, "/ui/", nil, alice).Body.String()
	if !strings.Contains(both, `<option value="kata">kata</option>`) || !strings.Contains(both, `<option value="gvisor" selected>`) {
		t.Error("runtime choice missing, or gvisor isn't the default")
	}
	rec := uiDo(h, http.MethodPost, "/ui/sandboxes", url.Values{
		"image": {"ubuntu-dev"}, "runtime": {"kata"}, "ssh_public_key": {"ssh-ed25519 AAAAkey"}, "ttl_hours": {"1"},
	}, alice)
	if rec.Code != http.StatusSeeOther || len(svc.created) != 1 || svc.created[0].Runtime != "kata" {
		t.Fatalf("create with kata = %d, requests %+v", rec.Code, svc.created)
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

	// The Go field name itself is not accepted from JSON (json:"-"), so a
	// body can't name another key.
	for _, field := range []string{"CreatedBy", "created_by"} {
		spoof := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(
			`{"image":"ubuntu-dev","ssh_public_key":"ssh-ed25519 AAAAkey","`+field+`":{"ID":"k-other","Name":"evil"}}`))
		spoof.Header.Set("Authorization", "Bearer "+goodKey)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, spoof)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create with %q in the body = %d, want 400", field, rec.Code)
		}
	}
	if len(svc.created) != 1 {
		t.Fatalf("spoofed creates went through: %+v", svc.created)
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
	// A key name is user input: escaped wherever it's shown.
	sb.rows["sb-alice0000001"] = func() store.Sandbox {
		s := sb.rows["sb-alice0000001"]
		s.CreatedByKey = &store.KeyRef{ID: "k1", Name: "<script>x</script>", Prefix: "sk_vOawHfdi"}
		return s
	}()
	for _, page := range []struct {
		path string
		c    *http.Cookie
	}{{"/ui/", alice}, {"/ui/sandboxes/sb-alice0000001", alice}, {"/ui/admin", root}} {
		body := uiDo(h, http.MethodGet, page.path, nil, page.c).Body.String()
		if strings.Contains(body, "<script>x</script>") || !strings.Contains(body, "&lt;script&gt;x&lt;/script&gt;") {
			t.Errorf("%s: key name not escaped", page.path)
		}
	}
	if got := keyLabel(&store.KeyRef{Prefix: "sk_abcdefgh"}); got != "sk_abcdefgh…" {
		t.Errorf("unnamed key label = %q", got)
	}
}

// On the owner's pages, "created by" links to the key on the keys page if
// it's still active, says "(revoked)" if not; elsewhere it's plain text.
func TestUICreatedByLinksToKey(t *testing.T) {
	sb := aliceAndBob()
	set := func(id string, k *store.KeyRef) {
		s := sb.rows[id]
		s.CreatedByKey = k
		sb.rows[id] = s
	}
	set("sb-alice0000001", &store.KeyRef{ID: "k-session", Name: "browser", Prefix: "sk_aliceAAA"}) // active (aliceKeys)
	set("sb-bob000000001", &store.KeyRef{ID: "k-bobs", Name: "bobs-agent", Prefix: "sk_bob00000"})
	h := adminUI(sb) // has Keys: aliceKeys()
	link := `<a href="/ui/keys#key-k-session">browser (sk_aliceAAA…)</a>`
	for _, path := range []string{"/ui/", "/ui/sandboxes/sb-alice0000001"} {
		if body := uiDo(h, http.MethodGet, path, nil, aliceSession).Body.String(); !strings.Contains(body, link) {
			t.Errorf("%s: no link to the active key", path)
		}
	}
	if body := uiDo(h, http.MethodGet, "/ui/keys", nil, aliceSession).Body.String(); !strings.Contains(body, `<tr id="key-k-session">`) {
		t.Error("keys page row has no anchor")
	}

	// The same sandbox after its key was revoked (no longer active).
	set("sb-alice0000001", &store.KeyRef{ID: "k-revoked", Name: "old-agent", Prefix: "sk_oldagent"})
	body := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-alice0000001", nil, aliceSession).Body.String()
	if !strings.Contains(body, `old-agent (sk_oldagent…) <span class="muted">(revoked)</span>`) || strings.Contains(body, "#key-k-revoked") {
		t.Error("revoked key not marked, or still linked")
	}

	// An admin looking at someone else's sandbox, and the admin list: plain
	// text (the keys page only has the viewer's own keys).
	for _, path := range []string{"/ui/sandboxes/sb-bob000000001", "/ui/admin"} {
		body := uiDo(h, http.MethodGet, path, nil, root).Body.String()
		if !strings.Contains(body, "bobs-agent (sk_bob00000…)") || strings.Contains(body, "#key-k-bobs") || strings.Contains(body, "(revoked)") {
			t.Errorf("%s as admin: want a plain label for bob's key", path)
		}
	}
}

// The key list is only used for the "created by" link, so if it can't be
// loaded the sandbox pages still work (plain labels), and without a keys
// service at all they show plain labels too.
func TestUISandboxPagesSurviveKeyListFailure(t *testing.T) {
	sb := aliceAndBob()
	s := sb.rows["sb-alice0000001"]
	s.CreatedByKey = &store.KeyRef{ID: "k-session", Name: "browser", Prefix: "sk_aliceAAA"}
	sb.rows["sb-alice0000001"] = s
	for name, keys := range map[string]KeyService{"list fails": failingListKeys{aliceKeys()}, "no keys service": nil} {
		h := NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{},
			Auth: uiAuth{}, Sandboxes: sb, Keys: keys})
		for _, path := range []string{"/ui/", "/ui/?ended=1", "/ui/sandboxes/sb-alice0000001"} {
			rec := uiDo(h, http.MethodGet, path, nil, aliceSession)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "browser (sk_aliceAAA…)") ||
				strings.Contains(rec.Body.String(), "#key-") {
				t.Errorf("%s, %s = %d; want 200 with a plain label", name, path, rec.Code)
			}
		}
	}
}
