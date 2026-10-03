package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// fakeAdmin serves the admin lists from an ownedSandboxes fake.
type fakeAdmin struct{ sb *ownedSandboxes }

func (f fakeAdmin) ListUsers(context.Context) ([]store.UserSummary, error) {
	return []store.UserSummary{
		{User: store.User{ID: "u1", Name: "alice", MaxSandboxes: 10, CreatedAt: time.Now()}, LiveSandboxes: 1, ActiveKeys: 2},
		{User: store.User{ID: "u2", Name: "root", IsAdmin: true, MaxSandboxes: 10, CreatedAt: time.Now()}},
		{User: store.User{ID: "u9", Name: "bob", MaxSandboxes: 3, CreatedAt: time.Now()}, LiveSandboxes: 1, ActiveKeys: 1},
	}, nil
}

func (f fakeAdmin) ListAllSandboxes(_ context.Context, includeEnded bool) ([]store.OwnedSandbox, error) {
	owners := map[string]string{"u1": "alice", "u9": "bob"}
	f.sb.mu.Lock()
	defer f.sb.mu.Unlock()
	var out []store.OwnedSandbox
	for _, s := range f.sb.rows {
		if includeEnded || store.IsLive(s.Status) {
			out = append(out, store.OwnedSandbox{Sandbox: s, Owner: owners[s.UserID]})
		}
	}
	return out, nil
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
	for _, path := range []string{"/ui/admin", "/ui/admin/rows", "/ui/admin?ended=1"} {
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
		`hx-get="/ui/admin/rows"`, `name="from" value="admin"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page missing %q", want)
		}
	}
	if strings.Contains(body, "sb-alice0000old") {
		t.Error("ended sandbox shown without ?ended=1")
	}
	if !strings.Contains(uiDo(h, http.MethodGet, "/ui/admin/rows?ended=1", nil, root).Body.String(), "sb-alice0000old") {
		t.Error("rows?ended=1 doesn't include ended sandboxes")
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

// An open admin page whose user lost admin rights leaves instead of
// showing a frozen table (htmx doesn't swap error responses).
func TestUIAdminRowsForDemotedAdmin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/rows", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(alice) // not an admin
	rec := httptest.NewRecorder()
	adminUI(aliceAndBob()).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || rec.Header().Get("HX-Redirect") != "/ui/" {
		t.Fatalf("= %d HX-Redirect %q, want 404 + HX-Redirect /ui/", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}

// manySandboxes returns more sandboxes than the admin list shows.
type manySandboxes struct{ fakeAdmin }

func (manySandboxes) ListAllSandboxes(context.Context, bool) ([]store.OwnedSandbox, error) {
	out := make([]store.OwnedSandbox, store.MaxAdminSandboxes+1)
	for i := range out {
		out[i] = store.OwnedSandbox{Sandbox: store.Sandbox{ID: fmt.Sprintf("sb-%012d", i), Status: store.StatusDeleted}, Owner: "x"}
	}
	return out, nil
}

func TestUIAdminSaysWhenListIsCut(t *testing.T) {
	h := NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{},
		Auth: uiAuth{}, Sandboxes: aliceAndBob(), Keys: aliceKeys(), Admin: manySandboxes{}})
	body := uiDo(h, http.MethodGet, "/ui/admin/rows?ended=1", nil, root).Body.String()
	if !strings.Contains(body, fmt.Sprintf("Showing the newest %d sandboxes only.", store.MaxAdminSandboxes)) {
		t.Fatal("no notice that the list was cut")
	}
	if strings.Contains(body, fmt.Sprintf("sb-%012d", store.MaxAdminSandboxes)) {
		t.Fatal("the extra row was shown")
	}
}

// When a refresh can no longer see its sandbox (e.g. a demoted admin on
// another user's detail page), the page leaves instead of freezing on stale
// data: htmx doesn't swap error responses, so a bare 404 would keep it.
func TestUIStatusRefreshLosesAccess(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ui/sandboxes/sb-bob000000001/status", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(alice) // not bob, not an admin
	rec := httptest.NewRecorder()
	adminUI(aliceAndBob()).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || rec.Header().Get("HX-Redirect") != "/ui/" {
		t.Fatalf("= %d HX-Redirect %q, want 404 + HX-Redirect /ui/", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}
