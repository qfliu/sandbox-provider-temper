package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// fakeAdmin serves the admin lists from an ownedSandboxes fake.
type fakeAdmin struct{ sb *ownedSandboxes }

func (f fakeAdmin) ListUsers(_ context.Context, limit, offset int) ([]store.UserSummary, int, error) {
	all := []store.UserSummary{
		{User: store.User{ID: "u1", Name: "alice", MaxSandboxes: 10, CreatedAt: time.Now()}, LiveSandboxes: 1, ActiveKeys: 2},
		{User: store.User{ID: "u2", Name: "root", IsAdmin: true, MaxSandboxes: 10, CreatedAt: time.Now()}},
		{User: store.User{ID: "u9", Name: "bob", MaxSandboxes: 3, CreatedAt: time.Now()}, LiveSandboxes: 1, ActiveKeys: 1},
	}
	return pageOf(all, limit, offset), len(all), nil
}

func (f fakeAdmin) ListAllSandboxes(_ context.Context, includeEnded bool, limit, offset int) ([]store.OwnedSandbox, int, error) {
	owners := map[string]string{"u1": "alice", "u9": "bob"}
	f.sb.mu.Lock()
	defer f.sb.mu.Unlock()
	var out []store.OwnedSandbox
	for _, s := range f.sb.rows {
		if includeEnded || store.IsLive(s.Status) {
			out = append(out, store.OwnedSandbox{Sandbox: s, Owner: owners[s.UserID]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return pageOf(out, limit, offset), len(out), nil
}

func adminUI(sb *ownedSandboxes) http.Handler {
	return NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{},
		Auth: uiAuth{}, Sandboxes: sb, Keys: aliceKeys(), Admin: fakeAdmin{sb}})
}

var root = &http.Cookie{Name: sessionCookie, Value: "sk_admin"}

// Non-admins get 404 on admin pages (not 403, so they don't learn the
// pages exist) and see no Admin link.
func TestUIAdminHiddenFromNonAdmins(t *testing.T) {
	h := adminUI(aliceAndBob())
	for _, path := range []string{"/ui/admin", "/ui/admin?ended=1", "/ui/admin?page=2"} {
		if rec := uiDo(h, http.MethodGet, path, nil, alice); rec.Code != http.StatusNotFound {
			t.Errorf("alice GET %s = %d, want 404", path, rec.Code)
		}
	}
	if body := uiDo(h, http.MethodGet, "/ui/", nil, alice).Body.String(); strings.Contains(body, `href="/ui/admin"`) {
		t.Error("Admin link shown to a non-admin")
	}
}

func TestUIAdminSeesEveryone(t *testing.T) {
	h := adminUI(aliceAndBob())
	body := uiDo(h, http.MethodGet, "/ui/admin", nil, root).Body.String()
	for _, want := range []string{
		"alice", "bob", "1 / 10", "1 / 3", // users with live/quota
		"sb-alice0000001", "sb-bob000000001", // everyone's live sandboxes
		`<a href="/ui/admin">Refresh</a>`, `name="from" value="admin"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page missing %q", want)
		}
	}
	if strings.Contains(body, "sb-alice0000old") {
		t.Error("ended sandbox shown without ?ended=1")
	}
	if !strings.Contains(uiDo(h, http.MethodGet, "/ui/admin?ended=1", nil, root).Body.String(), "sb-alice0000old") {
		t.Error("?ended=1 doesn't include ended sandboxes")
	}
	// The Users table itself rendered (names alone also match the owner
	// column): root has no sandboxes and an admin badge in that table.
	if !strings.Contains(body, "<td>root <span class=\"badge\">admin</span></td>") || !strings.Contains(body, "0 / 10") {
		t.Error("users table not rendered")
	}
	if !strings.Contains(uiDo(h, http.MethodGet, "/ui/", nil, root).Body.String(), `href="/ui/admin"`) {
		t.Error("no Admin link for an admin")
	}
	// An admin can open anyone's sandbox (the service allows it).
	if rec := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-bob000000001", nil, root); rec.Code != http.StatusOK {
		t.Errorf("admin viewing bob's sandbox = %d, want 200", rec.Code)
	}
}

func TestUIAdminDeletesAnySandbox(t *testing.T) {
	sb := aliceAndBob()
	h := adminUI(sb)
	rec := uiDo(h, http.MethodPost, "/ui/sandboxes/sb-bob000000001/delete", url.Values{"from": {"admin"}}, root)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/admin" {
		t.Fatalf("admin delete = %d → %q, want 303 → /ui/admin", rec.Code, rec.Header().Get("Location"))
	}
	if len(sb.deleted) != 1 || sb.deleted[0] != "sb-bob000000001" {
		t.Fatalf("deleted = %v", sb.deleted)
	}
	// "from" only selects between fixed pages; it's never used as a URL.
	rec = uiDo(h, http.MethodPost, "/ui/sandboxes/sb-alice0000001/delete", url.Values{"from": {"https://evil.example"}}, alice)
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Fatalf("from=<url> redirected to %q, want /ui/", loc)
	}
}

// Deleting someone else's sandbox from its detail page (no from field)
// returns an admin to the admin page, and the detail page links back there.
func TestUIAdminDeleteFromDetailReturnsToAdmin(t *testing.T) {
	sb := aliceAndBob()
	h := adminUI(sb)
	if body := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-bob000000001", nil, root).Body.String(); !strings.Contains(body, `href="/ui/admin">← Admin`) {
		t.Error("admin viewing another user's sandbox has no link back to the admin page")
	}
	rec := uiDo(h, http.MethodPost, "/ui/sandboxes/sb-bob000000001/delete", url.Values{}, root)
	if loc := rec.Header().Get("Location"); loc != "/ui/admin" {
		t.Fatalf("admin delete from detail → %q, want /ui/admin", loc)
	}
	if body := uiDo(h, http.MethodGet, "/ui/sandboxes/sb-alice0000001", nil, alice).Body.String(); strings.Contains(body, "← Admin") {
		t.Error("owner sees a link to the admin page")
	}
}
