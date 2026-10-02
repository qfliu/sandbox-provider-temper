-- Per-sandbox SSH host key (D29). The API generates a host key pair for each
-- sandbox, hands the private half to the pod via a Secret, and keeps the
-- public half here. The API and gateway only accept a connection whose host
-- key matches, so a reused pod IP can never lead into another sandbox.
ALTER TABLE sandboxes ADD COLUMN host_public_key text NOT NULL DEFAULT '';
