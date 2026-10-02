-- Initial schema: users, their API keys, and their sandboxes.

-- A user owns API keys and sandboxes (D17). Admins see everything.
CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL UNIQUE,
    is_admin      boolean     NOT NULL DEFAULT false,
    max_sandboxes integer     NOT NULL DEFAULT 10 CHECK (max_sandboxes >= 0),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- API keys are stored as a SHA-256 hash, never in plain text. Keys are 32
-- random bytes, so a fast hash is enough (bcrypt only matters for
-- low-entropy secrets like human passwords). `prefix` is the first few
-- characters of the key, shown in the dashboard so users can tell keys apart.
CREATE TABLE api_keys (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text        NOT NULL DEFAULT '',
    prefix       text        NOT NULL,
    key_hash     bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX api_keys_user_id ON api_keys (user_id);

-- One row per sandbox, kept after deletion for history and the admin view.
--   pending  → pod created, not ready yet
--   running  → sshd reachable
--   deleting → deletion requested, pod being removed
--   deleted  → gone (explicit delete, idle timeout or TTL)
--   failed   → pod could not start
--   lost     → pod or node disappeared unexpectedly (D8)
CREATE TABLE sandboxes (
    id               text        PRIMARY KEY,               -- e.g. sb-3f9a1c2b7d
    user_id          uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    image            text        NOT NULL CHECK (image IN ('ubuntu-base', 'ubuntu-dev')),  -- D2
    status           text        NOT NULL DEFAULT 'pending'
                                 CHECK (status IN ('pending', 'running', 'deleting', 'deleted', 'failed', 'lost')),
    status_reason    text        NOT NULL DEFAULT '',
    ssh_public_key   text        NOT NULL,                  -- D3: checked by the gateway
    pod_name         text        NOT NULL DEFAULT '',
    pod_ip           text        NOT NULL DEFAULT '',
    ttl_seconds      integer     NOT NULL CHECK (ttl_seconds > 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,                  -- D9: created_at + ttl
    last_activity_at timestamptz NOT NULL DEFAULT now(),    -- D9: idle timeout
    deleted_at       timestamptz
);
-- Quota checks and "my sandboxes" lists only look at live sandboxes.
CREATE INDEX sandboxes_user_live ON sandboxes (user_id)
    WHERE status NOT IN ('deleted', 'failed', 'lost');
-- The reaper scans live sandboxes by expiry.
CREATE INDEX sandboxes_live_expires ON sandboxes (expires_at)
    WHERE status IN ('pending', 'running');
