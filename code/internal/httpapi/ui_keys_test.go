package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// aliceLongKey is a session key for alice whose display prefix is
// "sk_aliceAAA".
const aliceLongKey = "sk_aliceAAAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// fakeKeys holds API keys per user and, like the store, refuses to revoke
// others' keys or a user's last one.
type fakeKeys struct {
	mu      sync.Mutex
	keys    []store.APIKey
	revoked []string
}

func (f *fakeKeys) ListAPIKeys(_ context.Context, userID string) ([]store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.APIKey
	for _, k := range f.keys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeKeys) CreateAPIKey(_ context.Context, userID, name string) (string, store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := store.APIKey{ID: "k-new", UserID: userID, Name: name, Prefix: "sk_NEWKEY00", CreatedAt: time.Now()}
	f.keys = append(f.keys, k)
	return "sk_NEWKEY00-the-secret-part-shown-once", k, nil
}

func (f *fakeKeys) RevokeAPIKey(_ context.Context, userID, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx, n := -1, 0
	for i, k := range f.keys {
		if k.UserID == userID {
			n++
			if k.ID == keyID {
				idx = i
			}
		}
	}
	if idx < 0 {
		return store.ErrNotFound
	}
	if n <= 1 {
		return store.ErrLastKey
	}
	f.revoked = append(f.revoked, keyID)
	f.keys = append(f.keys[:idx], f.keys[idx+1:]...)
	return nil
}

func aliceKeys() *fakeKeys {
	used := time.Now().Add(-5 * time.Minute)
	return &fakeKeys{keys: []store.APIKey{
		{ID: "k-session", UserID: "u1", Name: "browser", Prefix: "sk_aliceAAA", CreatedAt: time.Now().Add(-time.Hour), LastUsedAt: &used},
		{ID: "k-agent", UserID: "u1", Name: "agent", Prefix: "sk_agent111", CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "k-bob", UserID: "u9", Name: "bob's", Prefix: "sk_bobbbbbb", CreatedAt: time.Now()},
	}}
}

func keysUI(keys KeyService) http.Handler {
	return NewHandler(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{},
		Auth: uiAuth{}, Sandboxes: aliceAndBob(), Keys: keys})
}

var aliceSession = &http.Cookie{Name: sessionCookie, Value: aliceLongKey}

func TestUIKeysListsOwnKeys(t *testing.T) {
	body := uiDo(keysUI(aliceKeys()), http.MethodGet, "/ui/keys", nil, aliceSession).Body.String()
	for _, want := range []string{"browser", "sk_aliceAAA…", "agent", "sk_agent111…", "never", "5m ago",
		`action="/ui/keys/k-agent/revoke"`} {
		if !strings.Contains(body, want) {
			t.Errorf("keys page missing %q", want)
		}
	}
	if strings.Contains(body, "sk_bobbbbbb") {
		t.Error("another user's key listed")
	}
	// The key this session uses is marked, and revoking it warns about
	// signing out.
	if strings.Count(body, `class="badge">this session`) != 1 ||
		!strings.Contains(body, "This is the key you're signed in with") {
		t.Error("current session key not marked or not warned about")
	}
}

// A new key is shown in the create response only.
func TestUICreateKeyShownOnce(t *testing.T) {
	keys := aliceKeys()
	h := keysUI(keys)
	rec := uiDo(h, http.MethodPost, "/ui/keys", url.Values{"name": {"  ci-runner  "}}, aliceSession)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "sk_NEWKEY00-the-secret-part-shown-once") ||
		!strings.Contains(body, "won't be shown again") {
		t.Fatalf("create = %d, key not shown once:\n%s", rec.Code, body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("page with a new key may be cached")
	}
	if got := keys.keys[len(keys.keys)-1].Name; got != "ci-runner" {
		t.Fatalf("name = %q, want trimmed ci-runner", got)
	}
	if again := uiDo(h, http.MethodGet, "/ui/keys", nil, aliceSession).Body.String(); strings.Contains(again, "secret-part") {
		t.Fatal("new key shown again on a later visit")
	}
}

func TestUICreateKeyNameTooLong(t *testing.T) {
	keys := aliceKeys()
	rec := uiDo(keysUI(keys), http.MethodPost, "/ui/keys", url.Values{"name": {strings.Repeat("x", 101)}}, aliceSession)
	if rec.Code != http.StatusBadRequest || len(keys.keys) != 3 {
		t.Fatalf("= %d with %d keys, want 400 and no new key", rec.Code, len(keys.keys))
	}
}

func TestUIRevokeKey(t *testing.T) {
	keys := aliceKeys()
	h := keysUI(keys)
	if rec := uiDo(h, http.MethodPost, "/ui/keys/k-bob/revoke", url.Values{}, aliceSession); rec.Code != http.StatusNotFound {
		t.Errorf("revoking bob's key as alice = %d, want 404", rec.Code)
	}
	if rec := uiDo(h, http.MethodGet, "/ui/keys/k-agent/revoke", nil, aliceSession); rec.Code == http.StatusSeeOther {
		t.Error("revoke via GET worked")
	}
	rec := uiDo(h, http.MethodPost, "/ui/keys/k-agent/revoke", url.Values{}, aliceSession)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/keys" {
		t.Fatalf("revoke = %d → %q, want 303 → /ui/keys", rec.Code, rec.Header().Get("Location"))
	}
	if len(keys.revoked) != 1 || keys.revoked[0] != "k-agent" {
		t.Fatalf("revoked = %v", keys.revoked)
	}
	// Now the session's key is alice's only one.
	rec = uiDo(h, http.MethodPost, "/ui/keys/k-session/revoke", url.Values{}, aliceSession)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/keys?error=last-key" {
		t.Fatalf("revoking the last key = %d → %q, want 303 → /ui/keys?error=last-key", rec.Code, rec.Header().Get("Location"))
	}
	if body := uiDo(h, http.MethodGet, "/ui/keys?error=last-key", nil, aliceSession).Body.String(); !strings.Contains(body, "only API key") {
		t.Fatal("last-key explanation not shown")
	}
	// Only known codes produce a message; nothing from the URL is echoed.
	if body := uiDo(h, http.MethodGet, "/ui/keys?error=%3Cb%3Ehi", nil, aliceSession).Body.String(); strings.Contains(body, `role="alert"`) {
		t.Fatal("unknown error code shown")
	}
}

func TestUINavLinks(t *testing.T) {
	body := uiDo(keysUI(aliceKeys()), http.MethodGet, "/ui/", nil, aliceSession).Body.String()
	if !strings.Contains(body, `href="/ui/keys"`) {
		t.Fatal("no link to the keys page")
	}
}
