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
	TTLSeconds     int        `json:"ttl_seconds"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	LastActivityAt time.Time  `json:"last_activity_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}

// NewSandbox is what's needed to insert a sandbox row.
type NewSandbox struct {
	ID           string
	UserID       string
	Image        string
	SSHPublicKey string
	TTLSeconds   int
}

const sandboxColumns = `id, user_id::text, image, status, status_reason, ssh_public_key,
	pod_name, pod_ip, ttl_seconds, created_at, expires_at, last_activity_at, deleted_at`

func scanSandbox(row pgx.Row) (Sandbox, error) {
	var s Sandbox
	err := row.Scan(&s.ID, &s.UserID, &s.Image, &s.Status, &s.StatusReason, &s.SSHPublicKey,
		&s.PodName, &s.PodIP, &s.TTLSeconds, &s.CreatedAt, &s.ExpiresAt, &s.LastActivityAt, &s.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	return s, err
}

// CreateSandbox inserts a pending sandbox if the user is under quota.
//
// The user's row is locked (FOR UPDATE) while counting, so two concurrent
// requests can't both see "9 of 10" and end up at 11.
func (s *Store) CreateSandbox(ctx context.Context, n NewSandbox) (Sandbox, error) {
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
			INSERT INTO sandboxes (id, user_id, image, ssh_public_key, pod_name, ttl_seconds, expires_at)
			VALUES ($1, $2, $3, $4, $1, $5::int, now() + make_interval(secs => $5::int))
			RETURNING `+sandboxColumns,
			n.ID, n.UserID, n.Image, n.SSHPublicKey, n.TTLSeconds))
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
