-- Index the created-by foreign key (0005), so deleting an API key (e.g. a
-- future user deletion) doesn't scan every sandbox row ever kept to clear
-- the reference (review of #43).
CREATE INDEX sandboxes_created_by_key ON sandboxes (created_by_key_id)
    WHERE created_by_key_id IS NOT NULL;
