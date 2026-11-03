-- Reverses 0023. The avatar rows go; the objects stay in the store (orphans).
ALTER TABLE projects
    DROP CONSTRAINT projects_avatar_shape,
    DROP COLUMN avatar_version,
    DROP COLUMN avatar_content_type,
    DROP COLUMN avatar_key;
ALTER TABLE orgs
    DROP CONSTRAINT orgs_avatar_shape,
    DROP COLUMN avatar_version,
    DROP COLUMN avatar_content_type,
    DROP COLUMN avatar_key;
