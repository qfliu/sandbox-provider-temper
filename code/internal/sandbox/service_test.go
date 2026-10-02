package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/db"
	"github.com/qfliu/sandbox-provider-temper/code/internal/dbtest"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

const validKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEv765TaxvHJjfGEoUUNbzPT0zk8UzbLbMxNO5n7TRCo alice@laptop"

// fakePods records calls; set failCreate to simulate Kubernetes errors and
// onDelete to run something (like a watcher event) during a delete.
type fakePods struct {
	created, deleted []string
	hostKeys         map[string][]byte
	failCreate       bool
	onDelete         func(name string)
	secrets          []string // sandbox ids that have a host-key Secret
	cleanedSecrets   []string
}

func (f *fakePods) Create(_ context.Context, sb store.Sandbox, hostKeyPEM []byte) error {
	if f.failCreate {
		return errors.New("admission webhook said no")
	}
	f.created = append(f.created, sb.PodName)
	if f.hostKeys == nil {
		f.hostKeys = map[string][]byte{}
	}
	f.hostKeys[sb.ID] = hostKeyPEM
	return nil
}

func (f *fakePods) CleanupSecrets(_ context.Context, isLive func(string) bool) ([]string, error) {
	var deleted []string
	for _, id := range f.secrets {
		if !isLive(id) {
			deleted = append(deleted, id+"-hostkey")
		}
	}
	f.cleanedSecrets = deleted
	return deleted, nil
}

func (f *fakePods) Delete(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	if f.onDelete != nil {
		f.onDelete(name)
	}
	return nil
}

type fixture struct {
	svc               *Service
	pods              *fakePods
	st                *store.Store
	pool              *pgxpool.Pool
	alice, bob, admin store.User
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := dbtest.RawPool(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st := store.New(pool)
	pods := &fakePods{}
	cfg := config.Sandbox{DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour}
	f := fixture{
		svc:  NewService(st, pods, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))),
		pods: pods,
		st:   st,
		pool: pool,
	}
	f.alice, _ = st.CreateUser(ctx, "alice", false)
	f.bob, _ = st.CreateUser(ctx, "bob", false)
	f.admin, _ = st.CreateUser(ctx, "root", true)
	return f
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	sb, err := f.svc.Create(context.Background(), f.alice, CreateRequest{Image: "ubuntu-dev", SSHPublicKey: "  " + validKey + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sb.ID, "sb-") || len(sb.ID) != 15 {
		t.Fatalf("id = %q, want sb- + 12 hex chars", sb.ID)
	}
	if sb.TTLSeconds != 3600 || sb.SSHPublicKey != validKey || sb.Status != store.StatusPending {
		t.Fatalf("unexpected sandbox %+v", sb)
	}
	if len(f.pods.created) != 1 || f.pods.created[0] != sb.ID {
		t.Fatalf("pods created = %v", f.pods.created)
	}
	// The pod got the private host key whose public half is in the database.
	signer, err := ssh.ParsePrivateKey(f.pods.hostKeys[sb.ID])
	if err != nil {
		t.Fatalf("pod host key: %v", err)
	}
	stored, _ := f.st.GetSandbox(context.Background(), sb.ID)
	if got := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))); got != stored.HostPublicKey {
		t.Fatalf("pod host key %q doesn't match stored %q", got, stored.HostPublicKey)
	}
}

func TestCreateValidation(t *testing.T) {
	f := newFixture(t)
	cases := map[string]CreateRequest{
		"unknown image": {Image: "alpine", SSHPublicKey: validKey},
		"missing key":   {Image: "ubuntu-dev"},
		"not a key":     {Image: "ubuntu-dev", SSHPublicKey: "hello"},
		"two keys":      {Image: "ubuntu-dev", SSHPublicKey: validKey + "\n" + validKey},
		"ttl too short": {Image: "ubuntu-dev", SSHPublicKey: validKey, TTLSeconds: 10},
		"ttl too long":  {Image: "ubuntu-dev", SSHPublicKey: validKey, TTLSeconds: 25 * 3600},
		"negative ttl":  {Image: "ubuntu-dev", SSHPublicKey: validKey, TTLSeconds: -5},
	}
	for name, req := range cases {
		var verr *ValidationError
		if _, err := f.svc.Create(context.Background(), f.alice, req); !errors.As(err, &verr) {
			t.Errorf("%s: err = %v, want ValidationError", name, err)
		}
	}
	if len(f.pods.created) != 0 {
		t.Fatalf("invalid requests created pods: %v", f.pods.created)
	}
}

func TestCreatePodFailureMarksFailedAndFreesQuota(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pods.failCreate = true

	if _, err := f.svc.Create(ctx, f.alice, CreateRequest{Image: "ubuntu-base", SSHPublicKey: validKey}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	all, _ := f.st.ListSandboxes(ctx, f.alice.ID, true)
	if len(all) != 1 || all[0].Status != store.StatusFailed || !strings.Contains(all[0].StatusReason, "admission webhook") {
		t.Fatalf("want one failed sandbox with the reason, got %+v", all)
	}
	live, _ := f.st.ListSandboxes(ctx, f.alice.ID, false)
	if len(live) != 0 {
		t.Fatal("failed sandbox still counts as live")
	}
}

func TestOwnership(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb, _ := f.svc.Create(ctx, f.alice, CreateRequest{Image: "ubuntu-base", SSHPublicKey: validKey})

	if _, err := f.svc.Get(ctx, f.bob, sb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob get alice's sandbox: err = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.Delete(ctx, f.bob, sb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob delete alice's sandbox: err = %v, want ErrNotFound", err)
	}
	if len(f.pods.deleted) != 0 {
		t.Fatal("bob's failed delete still deleted the pod")
	}
	if _, err := f.svc.Get(ctx, f.admin, sb.ID); err != nil {
		t.Fatalf("admin get: %v", err)
	}
	got, err := f.svc.Delete(ctx, f.admin, sb.ID)
	if err != nil || got.Status != store.StatusDeleted || got.StatusReason != "deleted by admin root" {
		t.Fatalf("admin delete = %+v, %v", got, err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sb, _ := f.svc.Create(ctx, f.alice, CreateRequest{Image: "ubuntu-base", SSHPublicKey: validKey})

	first, err := f.svc.Delete(ctx, f.alice, sb.ID)
	if err != nil || first.Status != store.StatusDeleted || first.StatusReason != "deleted by owner" {
		t.Fatalf("delete = %+v, %v", first, err)
	}
	if _, err := f.svc.Delete(ctx, f.alice, sb.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if len(f.pods.deleted) != 1 {
		t.Fatalf("pod deleted %d times, want 1", len(f.pods.deleted))
	}
	if _, err := f.svc.Get(ctx, f.alice, "sb-doesnotexist"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id: err = %v", err)
	}
}
