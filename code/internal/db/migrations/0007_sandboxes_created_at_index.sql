-- The admin list (D43) is "newest first, up to 500", refreshed every 5 s per
-- open admin tab. Without an index that's a sequential scan plus a sort of
-- every sandbox ever kept (ended ones stay forever). This index matches the
-- ORDER BY exactly (created_at DESC, id), so Postgres reads the newest rows
-- and stops at the LIMIT. (Step 5.8, Phase 4 review.)
CREATE INDEX sandboxes_created_at ON sandboxes (created_at DESC, id);
