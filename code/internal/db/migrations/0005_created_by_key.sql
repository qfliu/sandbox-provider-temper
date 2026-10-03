-- Which API key created each sandbox (step 4.5), so a user with one key
-- per agent can tell which agent made it. The key's name and prefix are
-- copied onto the row at creation: the SSH gateway reads sandbox rows with
-- a role that can't read api_keys, and the label stays meaningful after the
-- key is revoked. All three are NULL for sandboxes created before this.
ALTER TABLE sandboxes
    ADD COLUMN created_by_key_id     uuid REFERENCES api_keys (id) ON DELETE SET NULL,
    ADD COLUMN created_by_key_name   text,
    ADD COLUMN created_by_key_prefix text;
