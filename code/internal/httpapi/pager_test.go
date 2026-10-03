package httpapi

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

func TestRequestedPageAndClamp(t *testing.T) {
	for raw, want := range map[string]int{"": 1, "abc": 1, "0": 1, "-3": 1, "2": 2, "99999999999": maxPageParam} {
		r := httptest.NewRequest(http.MethodGet, "/ui/?page="+raw, nil)
		if got := requestedPage(r, "page"); got != want {
			t.Errorf("?page=%q → %d, want %d", raw, got, want)
		}
	}
	for _, c := range []struct{ page, total, want int }{{1, 0, 1}, {5, 0, 1}, {3, 120, 3}, {4, 120, 3}, {2, 50, 1}, {2, 51, 2}} {
		if got := clampPage(c.page, c.total); got != c.want {
			t.Errorf("clampPage(%d, %d) = %d, want %d", c.page, c.total, got, c.want)
		}
	}
}

// Links: prev/next, first and last, two either side of the current page,
// "…" for gaps, and other query parameters kept.
func TestPagerLinks(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ui/admin?ended=1&page=6&upage=2", nil)
	p := newPager(r, "page", 6, 20*pageSize)
	var labels []string
	for _, l := range p.Links {
		labels = append(labels, l.Label)
		if l.N != 0 && (!strings.Contains(l.URL, "ended=1") || !strings.Contains(l.URL, "upage=2")) {
			t.Errorf("link %q loses other parameters: %s", l.Label, l.URL)
		}
	}
	if got := strings.Join(labels, " "); got != "← Prev 1 … 4 5 6 7 8 … 20 Next →" {
		t.Fatalf("links = %q", got)
	}
	if one := newPager(r, "page", 1, 10); one.Pages != 1 || len(one.Links) != 0 {
		t.Fatalf("a single page has links: %+v", one)
	}
}

// manyFor gives alice n live sandboxes, newest first by index.
func manyFor(n int) *ownedSandboxes {
	f := newOwnedSandboxes()
	base := time.Now()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sb-%012d", i)
		f.rows[id] = store.Sandbox{ID: id, UserID: "u1", Image: "ubuntu-base", Status: store.StatusRunning,
			CreatedAt: base.Add(-time.Duration(i) * time.Minute), ExpiresAt: base.Add(time.Hour)}
	}
	return f
}

// Every list shows 50 per page with page numbers; a bad or too-large page
// shows the last page (step 4.6).
func TestUIListPagination(t *testing.T) {
	h := sandboxUI(manyFor(120), "")
	p1 := uiDo(h, http.MethodGet, "/ui/", nil, alice).Body.String()
	if strings.Count(p1, `<tr>`) != 50+1 || !strings.Contains(p1, "Page 1 of 3 · 120 total") ||
		!strings.Contains(p1, "sb-000000000000") || strings.Contains(p1, "sb-000000000050") {
		t.Fatalf("page 1 wrong (rows %d)", strings.Count(p1, `<tr>`)-1)
	}
	p3 := uiDo(h, http.MethodGet, "/ui/?page=3", nil, alice).Body.String()
	if strings.Count(p3, `<tr>`) != 20+1 || !strings.Contains(p3, "sb-000000000119") || !strings.Contains(p3, "Page 3 of 3") {
		t.Fatalf("page 3 wrong (rows %d)", strings.Count(p3, `<tr>`)-1)
	}
	if big := uiDo(h, http.MethodGet, "/ui/?page=999", nil, alice).Body.String(); !strings.Contains(big, "Page 3 of 3") {
		t.Fatal("a too-large page isn't clamped to the last page")
	}
	if bad := uiDo(h, http.MethodGet, "/ui/?page=abc", nil, alice).Body.String(); !strings.Contains(bad, "Page 1 of 3") {
		t.Fatal("a malformed page isn't page 1")
	}
	if !strings.Contains(p3, `<a href="/ui/?page=3">Refresh</a>`) {
		t.Fatal("Refresh link doesn't keep the page")
	}
	few := uiDo(sandboxUI(manyFor(3), ""), http.MethodGet, "/ui/", nil, alice).Body.String()
	if strings.Contains(few, "Page 1 of") {
		t.Fatal("pager shown for a single page")
	}
}

// The admin page has two lists with independent pagers: ?upage= for users,
// ?page= for sandboxes.
func TestUIAdminPagination(t *testing.T) {
	h := adminUI(manyFor(60))
	body := uiDo(h, http.MethodGet, "/ui/admin?page=2", nil, root).Body.String()
	if !strings.Contains(body, "Page 2 of 2 · 60 total") || !strings.Contains(body, "sb-000000000055") ||
		strings.Contains(body, "sb-000000000005<") {
		t.Fatal("admin sandboxes page 2 wrong")
	}
	if strings.Contains(body, "Page 1 of 1") {
		t.Fatal("pager shown for the 3-user table")
	}
}

// The users table pages on its own parameter and keeps the sandboxes' page.
func TestUIAdminUsersPagination(t *testing.T) {
	sb := manyFor(60)
	h := NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{},
		Auth: uiAuth{}, Sandboxes: sb, Keys: aliceKeys(), Admin: fakeAdmin{sb: sb, extraUsers: 57}})
	body := uiDo(h, http.MethodGet, "/ui/admin?page=2&upage=2", nil, root).Body.String()
	if !strings.Contains(body, "Page 2 of 2 · 60 total") || strings.Count(body, "Page 2 of 2 · 60 total") != 2 {
		t.Fatal("want page 2 of both 60-row lists")
	}
	// users 51..60 are the last 10 extras (user-47 … user-56); user-46 is on page 1
	if !strings.Contains(body, "user-56") || !strings.Contains(body, "user-47") || strings.Contains(body, "user-46") {
		t.Fatal("users page 2 has the wrong rows")
	}
	if !strings.Contains(body, `href="/ui/admin?page=2&amp;upage=1"`) {
		t.Fatal("users pager link doesn't keep the sandboxes' page")
	}
}

func TestUIKeysPagination(t *testing.T) {
	keys := &fakeKeys{}
	for i := 0; i < 55; i++ {
		keys.keys = append(keys.keys, store.APIKey{ID: fmt.Sprint("k", i), UserID: "u1", Name: fmt.Sprint("key-", i),
			Prefix: fmt.Sprintf("sk_%08d", i), CreatedAt: time.Now()})
	}
	h := keysUI(keys)
	body := uiDo(h, http.MethodGet, "/ui/keys?page=2", nil, aliceSession).Body.String()
	if !strings.Contains(body, "Page 2 of 2 · 55 total") || !strings.Contains(body, "key-54") || strings.Contains(body, "key-4<") {
		t.Fatal("keys page 2 wrong")
	}
}

// Nothing in the dashboard refreshes on its own, and htmx is gone.
func TestUINoAutoRefresh(t *testing.T) {
	h := adminUI(aliceAndBob())
	for _, path := range []string{"/ui/", "/ui/?ended=1", "/ui/sandboxes/sb-alice0000001", "/ui/admin", "/ui/keys"} {
		c := alice
		if path == "/ui/admin" {
			c = root
		}
		body := uiDo(h, http.MethodGet, path, nil, c).Body.String()
		if strings.Contains(body, "hx-") || strings.Contains(body, "htmx") || !strings.Contains(body, ">Refresh</a>") {
			t.Errorf("%s: want a Refresh link and no htmx", path)
		}
	}
	if rec := uiDo(h, http.MethodGet, "/ui/static/htmx.min.js", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("htmx.min.js still served (%d)", rec.Code)
	}
}
