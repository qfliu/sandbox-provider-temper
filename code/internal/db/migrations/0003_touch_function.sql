-- touch_sandbox records activity for the idle reaper (D9, D32, D36) without
-- giving its caller write access to the sandboxes table. The SSH gateway's
-- role may EXECUTE only this; a plain UPDATE (last_activity_at) grant would
-- let it write any value — e.g. '-infinity', making the reaper delete every
-- sandbox (review finding on PR #27).
--
-- It can only set last_activity_at to now(), only on a running sandbox, and
-- at most every 10 seconds per sandbox.
--
-- SECURITY DEFINER runs it with the owner's rights; SET search_path FROM
-- CURRENT pins the schema it was created in, so a caller can't point it at
-- a look-alike table.
CREATE FUNCTION touch_sandbox(sandbox_id text) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path FROM CURRENT AS $$
    UPDATE sandboxes SET last_activity_at = now()
    WHERE id = sandbox_id
      AND status = 'running'
      AND last_activity_at < now() - interval '10 seconds';
$$;

-- New functions are executable by everyone by default; only grant it
-- explicitly (deploy-gateway.sh grants it to the gateway's role).
REVOKE ALL ON FUNCTION touch_sandbox(text) FROM PUBLIC;
