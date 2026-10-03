// Package store holds the SQL queries for users, API keys and sandboxes.
// Callers work with plain Go structs; nothing outside this package writes SQL.
package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/qfliu/sandbox-provider-temper/code/internal/auth"
)

var (
	// ErrNotFound means no matching row (or, for API keys, a revoked key).
	ErrNotFound = errors.New("not found")
	// ErrLastKey means revoking the key would leave the user with no API
	// key, locked out of the API and the dashboard.
	ErrLastKey = errors.New("can't revoke the only remaining API key")
	// ErrConflict means a unique value (like a user name) is already taken.
	ErrConflict = errors.New("already exists")
)

// Store runs queries against Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store using an already-migrated pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// User is an account that owns API keys and sandboxes (D17).
type User struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	IsAdmin      bool      `json:"is_admin"`
	MaxSandboxes int       `json:"max_sandboxes"`
	CreatedAt    time.Time `json:"created_at"`
}

// APIKey describes a key without its secret part.
type APIKey struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

const userColumns = "id::text, name, is_admin, max_sandboxes, created_at"

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.IsAdmin, &u.MaxSandboxes, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// CreateUser adds a user. Returns ErrConflict if the name is taken.
func (s *Store) CreateUser(ctx context.Context, name string, isAdmin bool) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		"INSERT INTO users (name, is_admin) VALUES ($1, $2) RETURNING "+userColumns,
		name, isAdmin))
	if isUniqueViolation(err) {
		return User{}, ErrConflict
	}
	return u, err
}

// UserByName looks a user up by name.
func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		"SELECT "+userColumns+" FROM users WHERE name = $1", name))
}

// CreateAPIKey issues a new key for a user. The returned plaintext is the
// only copy of the key; only its hash is stored.
func (s *Store) CreateAPIKey(ctx context.Context, userID, name string) (plaintext string, key APIKey, err error) {
	plaintext, prefix, hash := auth.NewKey()
	err = s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (user_id, name, prefix, key_hash) VALUES ($1, $2, $3, $4)
		RETURNING id::text, user_id::text, name, prefix, created_at, last_used_at`,
		userID, name, prefix, hash,
	).Scan(&key.ID, &key.UserID, &key.Name, &key.Prefix, &key.CreatedAt, &key.LastUsedAt)
	if err != nil {
		return "", APIKey{}, err
	}
	return plaintext, key, nil
}

// ListAPIKeys returns a user's active (not revoked) keys, newest first.
func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, user_id::text, name, prefix, created_at, last_used_at
		FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// RevokeAPIKey revokes one of the user's active keys. It's ErrNotFound if
// the key isn't theirs or is already revoked, and ErrLastKey if it's their
// only active key. The user's row is locked, so two concurrent revokes of
// a user's last two keys can't both succeed.
func (s *Store) RevokeAPIKey(ctx context.Context, userID, keyID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT 1 FROM users WHERE id = $1 FOR UPDATE", userID); err != nil {
			return err
		}
		var active int
		var isActive bool
		// id::text = $2 so a malformed id is just "not found", not an error.
		if err := tx.QueryRow(ctx, `
			SELECT count(*), coalesce(bool_or(id::text = $2), false)
			FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL`, userID, keyID,
		).Scan(&active, &isActive); err != nil {
			return err
		}
		if !isActive {
			return ErrNotFound
		}
		if active <= 1 {
			return ErrLastKey
		}
		_, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now()
			WHERE user_id = $1 AND id::text = $2 AND revoked_at IS NULL`, userID, keyID)
		return err
	})
}

// UserByAPIKey returns the owner of a valid (not revoked) key, or
// ErrNotFound. It also records when the key was last used, at most once a
// minute per key, so busy agents don't turn every request into a write.
func (s *Store) UserByAPIKey(ctx context.Context, plaintext string) (User, error) {
	if !auth.LooksLikeKey(plaintext) {
		return User{}, ErrNotFound
	}
	var keyID string
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT k.id::text, u.id::text, u.name, u.is_admin, u.max_sandboxes, u.created_at
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = $1 AND k.revoked_at IS NULL`,
		auth.Hash(plaintext),
	).Scan(&keyID, &u.ID, &u.Name, &u.IsAdmin, &u.MaxSandboxes, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}

	// Recording last_used_at is bookkeeping: if it fails (lock contention, a
	// read-only failover…), log it and still authenticate the valid key
	// (review finding M8). Its own short timeout keeps a slow write from
	// delaying the request.
	touchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(touchCtx, `
		UPDATE api_keys SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`,
		keyID); err != nil {
		slog.Warn("could not record API key use", "key_id", keyID, "err", err)
	}
	return u, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
