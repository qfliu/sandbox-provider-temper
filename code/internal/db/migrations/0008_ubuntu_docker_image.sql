-- The ubuntu-docker image (D62): sandboxes that run Docker. The CHECK from
-- 0001 lists the allowed images; widen it. (Postgres named the inline CHECK
-- sandboxes_image_check.)
ALTER TABLE sandboxes DROP CONSTRAINT sandboxes_image_check;
ALTER TABLE sandboxes ADD CONSTRAINT sandboxes_image_check
    CHECK (image IN ('ubuntu-base', 'ubuntu-dev', 'ubuntu-docker'));
