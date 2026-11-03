-- LAUN-22: an avatar (logo image) for an org and for a project. The image is
-- an object in the store; the row keeps its key, its type and a version that
-- goes up on every set and clear (cache busting and the ETag).
ALTER TABLE orgs
    ADD COLUMN avatar_key text,
    ADD COLUMN avatar_content_type text,
    ADD COLUMN avatar_version integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT orgs_avatar_shape CHECK (
        (avatar_key IS NULL) = (avatar_content_type IS NULL)
        AND avatar_version >= 0
        AND (avatar_content_type IS NULL OR avatar_content_type IN ('image/png', 'image/jpeg', 'image/gif', 'image/webp'))
    );

ALTER TABLE projects
    ADD COLUMN avatar_key text,
    ADD COLUMN avatar_content_type text,
    ADD COLUMN avatar_version integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT projects_avatar_shape CHECK (
        (avatar_key IS NULL) = (avatar_content_type IS NULL)
        AND avatar_version >= 0
        AND (avatar_content_type IS NULL OR avatar_content_type IN ('image/png', 'image/jpeg', 'image/gif', 'image/webp'))
    );
