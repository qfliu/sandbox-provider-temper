-- Runtime per sandbox (D65): gVisor (the default) or Kata Containers. Existing
-- rows were all gVisor.
ALTER TABLE sandboxes ADD COLUMN runtime text NOT NULL DEFAULT 'gvisor'
    CHECK (runtime IN ('gvisor', 'kata'));
