package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrQuotaExceeded means the user already has max_sandboxes live sandboxes.
var ErrQuotaExceeded = errors.New("sandbox quota exceeded")

// Sandbox statuses (see 0001_init.sql for what each means).
const (
	StatusPending  = "pending"
	StatusRunning  = "running"
	StatusDeleting = "deleting"
	StatusDeleted  = "deleted"
	StatusFailed   = "failed"
	StatusLost     = "lost"
)

// IsLive reports whether a sandbox still holds (or may hold) a pod, which
// is what counts toward the quota.
func IsLive(status string) bool {
	return status == StatusPending || status == StatusRunning || status == StatusDeleting
}

// Sandbox is one row of the sandboxes table. Pod name and IP are cluster
// internals, so they're kept out of API responses.
type Sandbox struct {
	ID             string     `json:"id"`
	UserID         string     `json:"user_id"`
	Image          string     `json:"image"`
	Status         string     `json:"status"`
	StatusReason   string     `json:"status_reason,omitempty"`
	SSHPublicKey   string     `json:"ssh_public_key"`
	PodName        string     `json:"-"`
	PodIP          string     `json:"-"`
	HostPublicKey  string     `json:"-"` // D29: the only host key the API/gateway accept
	TTLSeconds     int        `json:"ttl_seconds"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	LastActivityAt time.Time  `json:"last_activity_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
	// CreatedByKey is the API key that created the sandbox (step 4.5); nil
	// for sandboxes created before that was recorded.
	CreatedByKey *KeyRef `json:"created_by_key,omitempty"`
}

// KeyRef identifies an API key without its secret: enough to tell a
// user's keys (and so their agents) apart.
type KeyRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

// NewSandbox is what's needed to insert a sandbox row.
type NewSandbox struct {
	ID            string
	UserID        string
	Image         string
	SSHPublicKey  string
	HostPublicKey string
	TTLSeconds    int
	CreatedByKey  *KeyRef // the key that made the request; optional
}

// lastActivity is a sandbox's most recent use: API calls (its own
// last_activity_at) or open SSH sessions (ssh_activity, written by the
// gateway, D36). An SSH time is capped at now(), so a bogus future value
// can't break reads; GREATEST ignores a missing (NULL) SSH row and can't be
// pulled down by an ancient one.
const lastActivity = `GREATEST(sandboxes.last_activity_at,
	(SELECT LEAST(a.last_seen_at, now()) FROM ssh_activity a WHERE a.sandbox_id = sandboxes.id))`

const sandboxColumns = `id, user_id::text, image, status, status_reason, ssh_public_key,
	pod_name, pod_ip, host_public_key, ttl_seconds, created_at, expires_at, ` + lastActivity + `, deleted_at,
	created_by_key_id::text, created_by_key_name, created_by_key_prefix`

// sandboxScan scans sandboxColumns into s; call finish after a successful
// Scan to fill CreatedByKey. Extra destinations go after the columns.
type sandboxScan struct {
	s                         *Sandbox
	keyID, keyName, keyPrefix *string
}

func newSandboxScan(s *Sandbox) *sandboxScan {
	return &sandboxScan{s: s, keyID: new(string), keyName: new(string), keyPrefix: new(string)}
}

func (sc *sandboxScan) dest(extra ...any) []any {
	s := sc.s
	return append([]any{&s.ID, &s.UserID, &s.Image, &s.Status, &s.StatusReason, &s.SSHPublicKey,
		&s.PodName, &s.PodIP, &s.HostPublicKey, &s.TTLSeconds, &s.CreatedAt, &s.ExpiresAt, &s.LastActivityAt,
		&s.DeletedAt, &sc.keyID, &sc.keyName, &sc.keyPrefix}, extra...)
}

// finish fills CreatedByKey. The copied prefix is what counts: if the key
// row is ever deleted, the FK clears only the id, and the label stays.
func (sc *sandboxScan) finish() {
	if sc.keyID != nil || sc.keyPrefix != nil {
		sc.s.CreatedByKey = &KeyRef{ID: deref(sc.keyID), Name: deref(sc.keyName), Prefix: deref(sc.keyPrefix)}
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func scanSandbox(row pgx.Row) (Sandbox, error) {
	var s Sandbox
	sc := newSandboxScan(&s)
	err := row.Scan(sc.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err == nil {
		sc.finish()
	}
	return s, err
}

// CreateSandbox inserts a pending sandbox if the user is under quota.
//
// The user's row is locked (FOR UPDATE) while counting, so two concurrent
// requests can't both see "9 of 10" and end up at 11.
func (s *Store) CreateSandbox(ctx context.Context, n NewSandbox) (Sandbox, error) {
	var keyID, keyName, keyPrefix *string // NULL when no key is given
	if k := n.CreatedByKey; k != nil {
		keyID, keyName, keyPrefix = &k.ID, &k.Name, &k.Prefix
	}
	var sb Sandbox
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var max, live int
		if err := tx.QueryRow(ctx,
			"SELECT max_sandboxes FROM users WHERE id = $1 FOR UPDATE", n.UserID,
		).Scan(&max); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sandboxes
			WHERE user_id = $1 AND status IN ('pending', 'running', 'deleting')`, n.UserID,
		).Scan(&live); err != nil {
			return err
		}
		if live >= max {
			return ErrQuotaExceeded
		}
		var err error
		sb, err = scanSandbox(tx.QueryRow(ctx, `
			INSERT INTO sandboxes (id, user_id, image, ssh_public_key, host_public_key, pod_name, ttl_seconds, expires_at,
				created_by_key_id, created_by_key_name, created_by_key_prefix)
			VALUES ($1, $2, $3, $4, $5, $1, $6::int, now() + make_interval(secs => $6::int), $7::uuid, $8, $9)
			RETURNING `+sandboxColumns,
			n.ID, n.UserID, n.Image, n.SSHPublicKey, n.HostPublicKey, n.TTLSeconds, keyID, keyName, keyPrefix))
		return err
	})
	return sb, err
}

// GetSandbox returns one sandbox by id.
func (s *Store) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	return scanSandbox(s.pool.QueryRow(ctx,
		"SELECT "+sandboxColumns+" FROM sandboxes WHERE id = $1", id))
}

// ListSandboxes returns a user's sandboxes, newest first. Ended ones
// (deleted, failed, lost) are only included when includeEnded is set.
func (s *Store) ListSandboxes(ctx context.Context, userID string, includeEnded bool) ([]Sandbox, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+sandboxColumns+` FROM sandboxes
		WHERE user_id = $1 AND ($2 OR status IN ('pending', 'running', 'deleting'))
		ORDER BY created_at DESC`, userID, includeEnded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Sandbox{}
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, sb)
	}
	return list, rows.Err()
}

// MarkSandboxEnded moves a live sandbox to a final status (deleted, failed
// or lost) with a human-readable reason. Already-ended sandboxes are left
// as they are, so the first recorded reason wins.
func (s *Store) MarkSandboxEnded(ctx context.Context, id, status, reason string) (Sandbox, error) {
	sb, err := scanSandbox(s.pool.QueryRow(ctx, `
		UPDATE sandboxes SET status = $2, status_reason = $3, deleted_at = now()
		WHERE id = $1 AND status IN ('pending', 'running', 'deleting')
		RETURNING `+sandboxColumns, id, status, reason))
	if errors.Is(err, ErrNotFound) {
		return s.GetSandbox(ctx, id)
	}
	return sb, err
}

// MarkSandboxRunning records that a sandbox's pod is ready and where it is.
// Only pending/running rows change, so a late event can't revive a sandbox
// that is being deleted or has ended.
func (s *Store) MarkSandboxRunning(ctx context.Context, id, podIP string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sandboxes SET status = 'running', pod_ip = $2
		WHERE id = $1 AND status IN ('pending', 'running')
		AND (status <> 'running' OR pod_ip <> $2)`, id, podIP)
	return err
}

// MarkSandboxDeleting flags a sandbox whose pod is about to be deleted on
// purpose, so the pod disappearing isn't mistaken for a lost sandbox.
func (s *Store) MarkSandboxDeleting(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sandboxes SET status = 'deleting'
		WHERE id = $1 AND status IN ('pending', 'running')`, id)
	return err
}

// ListLiveSandboxes returns every live sandbox across all users.
func (s *Store) ListLiveSandboxes(ctx context.Context) ([]Sandbox, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+sandboxColumns+` FROM sandboxes
		WHERE status IN ('pending', 'running', 'deleting')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Sandbox{}
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, sb)
	}
	return list, rows.Err()
}

// TouchSandbox records API activity (exec, file transfer) for the idle
// reaper on a running sandbox. Writes at most every 10 seconds per
// sandbox, so a busy agent doesn't turn every call into a database write.
func (s *Store) TouchSandbox(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sandboxes SET last_activity_at = now()
		WHERE id = $1 AND status = 'running'
		  AND last_activity_at < now() - interval '10 seconds'`, id)
	return err
}

// RecordSSHActivity records that an SSH session on the sandbox is open
// (D36). Used by the gateway, whose database role may write ssh_activity
// but not the sandboxes table. At most one write every 10 seconds per
// sandbox.
func (s *Store) RecordSSHActivity(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO ssh_activity (sandbox_id, last_seen_at) VALUES ($1, now())
		ON CONFLICT (sandbox_id) DO UPDATE SET last_seen_at = now()
		WHERE ssh_activity.last_seen_at < now() - interval '10 seconds'`, id)
	return err
}

// ClaimForReaping atomically picks live sandboxes that are past their TTL or
// idle longer than idleFor, marks them "deleting" with the reason, and
// returns them. Because the claim is a single UPDATE, several API replicas
// reaping at once never claim the same sandbox twice. The reason is stored
// now so whoever finishes the delete (the reaper, or the reconcile after a
// failure) records it.
func (s *Store) ClaimForReaping(ctx context.Context, idleFor time.Duration, idleReason string) ([]Sandbox, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE sandboxes SET status = 'deleting', status_reason = CASE
			WHEN expires_at <= now() THEN 'expired: TTL reached'
			ELSE $2 END
		WHERE status IN ('pending', 'running')
		  AND (expires_at <= now() OR `+lastActivity+` <= now() - make_interval(secs => $1::double precision))
		RETURNING `+sandboxColumns, idleFor.Seconds(), idleReason)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Sandbox{}
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, sb)
	}
	return list, rows.Err()
}

// OwnedSandbox is a sandbox with its owner's name (admin view).
type OwnedSandbox struct {
	Sandbox
	Owner string
}

// MaxAdminSandboxes caps the admin list (ended sandboxes accumulate).
// ListAllSandboxes returns one more, so callers can tell the list was cut.
const MaxAdminSandboxes = 500

// ListAllSandboxes returns every user's sandboxes, newest first, with
// owner names; ended ones only if includeEnded (admin view, D43).
func (s *Store) ListAllSandboxes(ctx context.Context, includeEnded bool) ([]OwnedSandbox, error) {
	// Two separate statements rather than "$1 OR status IN (…)": a prepared
	// statement can switch to one generic plan for both cases, and the live
	// view would then walk the whole created_at index (review of #47). Each
	// text now gets its own plan: sandboxes_created_at for all, the live
	// partial index for live ones.
	where := "WHERE status IN ('pending', 'running', 'deleting')"
	if includeEnded {
		where = ""
	}
	rows, err := s.pool.Query(ctx, "SELECT "+sandboxColumns+`,
			(SELECT name FROM users WHERE users.id = sandboxes.user_id)
		FROM sandboxes `+where+`
		ORDER BY created_at DESC, id LIMIT $1`, MaxAdminSandboxes+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []OwnedSandbox{}
	for rows.Next() {
		var o OwnedSandbox
		sc := newSandboxScan(&o.Sandbox)
		if err := rows.Scan(sc.dest(&o.Owner)...); err != nil {
			return nil, err
		}
		sc.finish()
		list = append(list, o)
	}
	return list, rows.Err()
}
