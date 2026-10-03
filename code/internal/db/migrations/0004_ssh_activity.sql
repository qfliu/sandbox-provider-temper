-- SSH activity gets its own table, and touch_sandbox goes away (D36, Phase 3
-- batch review).
--
-- touch_sandbox (0003) was SECURITY DEFINER: every call by the gateway ran
-- with its owner's rights, i.e. the platform's (superuser) database user.
-- The batch review showed a caller's temporary table could hijack it and run
-- code as that superuser. Instead of patching it, the gateway no longer
-- borrows anyone's rights: it may write this table and nothing else, and the
-- reaper counts a sandbox as active if either the API's last_activity_at or
-- this table's last_seen_at is recent.
--
-- Worst case with stolen gateway credentials: fake "still in use" rows,
-- which keep a sandbox alive only until its TTL. The readers clamp
-- last_seen_at to now() and take the newer of the two times, so writing a
-- far-future or ancient value can't break reads or cause a deletion.
CREATE TABLE ssh_activity (
    sandbox_id   text        PRIMARY KEY REFERENCES sandboxes (id) ON DELETE CASCADE,
    last_seen_at timestamptz NOT NULL
);

DROP FUNCTION touch_sandbox(text);
