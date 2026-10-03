// Package sandbox is the business logic for sandboxes: validating requests,
// enforcing ownership and quota, and keeping the database and the pods in
// step. HTTP (and later the dashboard) call this; it calls store and k8s.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// Images is the fixed set of images users may pick from (D2).
var Images = []string{"ubuntu-base", "ubuntu-dev"}

const minTTL = time.Minute

// ValidationError is a problem with the caller's request (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// ErrUnavailable means Kubernetes refused or failed a pod operation (HTTP
// 503). On create the row is marked failed; on delete it stays "deleting"
// and the reconcile retries.
var ErrUnavailable = errors.New("kubernetes unavailable")

// Pods starts and stops sandbox pods (implemented by k8s.Pods; faked in tests).
type Pods interface {
	// Create starts the pod with hostKeyPEM as its SSH host key (D29).
	Create(ctx context.Context, sb store.Sandbox, hostKeyPEM []byte) error
	Delete(ctx context.Context, podName string) error
	// CleanupSecrets deletes host-key Secrets of sandboxes that aren't live.
	CleanupSecrets(ctx context.Context, isLive func(sandboxID string) bool) ([]string, error)
}

// Service implements sandbox operations.
type Service struct {
	store *store.Store
	pods  Pods
	ssh   *sshx.Pool
	cfg   config.Sandbox
	log   *slog.Logger
}

// NewService returns a Service. sshPool connects to sandboxes for exec and
// file transfer.
func NewService(s *store.Store, pods Pods, sshPool *sshx.Pool, cfg config.Sandbox, log *slog.Logger) *Service {
	return &Service{store: s, pods: pods, ssh: sshPool, cfg: cfg, log: log}
}

// CreateRequest is the body of POST /v1/sandboxes.
type CreateRequest struct {
	Image        string `json:"image"`
	SSHPublicKey string `json:"ssh_public_key"`
	// TTLSeconds is optional; 0 means the default (1 hour).
	TTLSeconds int `json:"ttl_seconds"`
	// CreatedBy is the API key making the request, set by the HTTP layer
	// from authentication, never from the request body (step 4.5).
	CreatedBy *store.KeyRef `json:"-"`
}

// Create validates the request, reserves quota by inserting the row, then
// creates the pod. If the pod can't be created the row is marked failed, so
// it stops counting toward the quota.
func (s *Service) Create(ctx context.Context, user store.User, req CreateRequest) (store.Sandbox, error) {
	if err := s.validateImage(req.Image); err != nil {
		return store.Sandbox{}, err
	}
	key, err := normalizeSSHKey(req.SSHPublicKey)
	if err != nil {
		return store.Sandbox{}, err
	}
	ttl, err := s.ttl(req.TTLSeconds)
	if err != nil {
		return store.Sandbox{}, err
	}
	// The sandbox's own SSH host key: the private half goes to the pod, the
	// public half to the database, so connections can verify they reached
	// this sandbox and no other (D29).
	hostKeyPEM, hostPub, err := sshx.GenerateHostKey()
	if err != nil {
		return store.Sandbox{}, err
	}

	sb, err := s.store.CreateSandbox(ctx, store.NewSandbox{
		ID:            newID(),
		UserID:        user.ID,
		Image:         req.Image,
		SSHPublicKey:  key,
		HostPublicKey: hostPub,
		TTLSeconds:    int(ttl / time.Second),
		CreatedByKey:  req.CreatedBy,
	})
	if err != nil {
		return store.Sandbox{}, err
	}

	if err := s.pods.Create(ctx, sb, hostKeyPEM); err != nil {
		s.log.Error("create sandbox pod", "sandbox", sb.ID, "err", err)
		// Use a fresh context: if the client hung up, still record the failure.
		if _, markErr := s.store.MarkSandboxEnded(context.WithoutCancel(ctx), sb.ID,
			store.StatusFailed, "pod creation failed: "+err.Error()); markErr != nil {
			s.log.Error("mark sandbox failed", "sandbox", sb.ID, "err", markErr)
		}
		return store.Sandbox{}, ErrUnavailable
	}
	s.log.Info("sandbox created", "sandbox", sb.ID, "user", user.Name, "image", sb.Image, "ttl", ttl)
	return sb, nil
}

// Get returns a sandbox the user may see: their own, or any for an admin.
// Others' sandboxes look like they don't exist (store.ErrNotFound), so ids
// can't be probed.
func (s *Service) Get(ctx context.Context, user store.User, id string) (store.Sandbox, error) {
	sb, err := s.store.GetSandbox(ctx, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	if sb.UserID != user.ID && !user.IsAdmin {
		return store.Sandbox{}, store.ErrNotFound
	}
	return sb, nil
}

// List returns the user's sandboxes; ended ones only if includeEnded.
func (s *Service) List(ctx context.Context, user store.User, includeEnded bool) ([]store.Sandbox, error) {
	return s.store.ListSandboxes(ctx, user.ID, includeEnded)
}

// Delete removes the pod and marks the sandbox deleted. Deleting an
// already-ended sandbox is a no-op that returns it unchanged.
func (s *Service) Delete(ctx context.Context, user store.User, id string) (store.Sandbox, error) {
	sb, err := s.Get(ctx, user, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	if !store.IsLive(sb.Status) {
		return sb, nil
	}
	// Finish the delete even if the client hangs up mid-way; a half-done
	// delete would leave the pod running while the row says "deleting".
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	// Mark first, so the status syncer sees an intended deletion rather
	// than a lost pod when the pod disappears.
	if err := s.store.MarkSandboxDeleting(ctx, sb.ID); err != nil {
		return store.Sandbox{}, err
	}
	if err := s.pods.Delete(ctx, sb.PodName); err != nil {
		// The row stays "deleting"; the reconcile retries the pod delete.
		s.log.Error("delete sandbox pod", "sandbox", sb.ID, "err", err)
		return store.Sandbox{}, ErrUnavailable
	}
	s.ssh.Drop(sb.ID)
	reason := "deleted by owner"
	if sb.UserID != user.ID {
		reason = "deleted by admin " + user.Name
	}
	s.log.Info("sandbox deleted", "sandbox", sb.ID, "by", user.Name)
	return s.store.MarkSandboxEnded(ctx, sb.ID, store.StatusDeleted, reason)
}

func (s *Service) validateImage(image string) error {
	for _, img := range Images {
		if image == img {
			return nil
		}
	}
	return &ValidationError{fmt.Sprintf("image must be one of %s", strings.Join(Images, ", "))}
}

func (s *Service) ttl(seconds int) (time.Duration, error) {
	if seconds == 0 {
		return s.cfg.DefaultTTL, nil
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl < minTTL || ttl > s.cfg.MaxTTL {
		return 0, &ValidationError{fmt.Sprintf("ttl_seconds must be between %d and %d",
			int(minTTL/time.Second), int(s.cfg.MaxTTL/time.Second))}
	}
	return ttl, nil
}

// normalizeSSHKey checks the key parses as an OpenSSH public key (like a
// line of authorized_keys) and returns it in canonical form, comment kept.
func normalizeSSHKey(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", &ValidationError{"ssh_public_key is required"}
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(raw))
	if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
		return "", &ValidationError{"ssh_public_key must be a single OpenSSH public key, e.g. the contents of ~/.ssh/id_ed25519.pub"}
	}
	// The gateway compares raw key bytes and enforces no authorized_keys
	// options or certificate rules, so don't accept either rather than
	// silently ignore them.
	if len(options) > 0 {
		return "", &ValidationError{"ssh_public_key must not have authorized_keys options (like from=)"}
	}
	if _, isCert := key.(*ssh.Certificate); isCert {
		return "", &ValidationError{"ssh_public_key must be a plain public key, not a certificate"}
	}
	out := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if comment != "" {
		out += " " + comment
	}
	return out, nil
}

// newID returns a sandbox id like "sb-3f9a1c2b7d4e": short enough to type in
// `ssh sb-…@gateway`, and a valid pod name. 48 random bits make collisions
// vanishingly unlikely; the primary key would reject one anyway.
func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "sb-" + hex.EncodeToString(b)
}
