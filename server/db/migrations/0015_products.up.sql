-- LAUN-62 (T18, decisions-T18-products #10): products, a named group of
-- teams, and the many-to-many product_teams (the shape of project_teams).

CREATE TABLE products (
    id          uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id      uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    slug        text        NOT NULL,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    icon        text,
    color       text,
    version     integer     NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz,
    CONSTRAINT products_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT products_org_id_slug_key UNIQUE (org_id, slug),
    CONSTRAINT products_name_check CHECK (char_length(name) BETWEEN 1 AND 200),
    -- Starts with a letter; no double and no trailing hyphen.
    CONSTRAINT products_slug_format_check CHECK (char_length(slug) BETWEEN 1 AND 40
        AND slug ~ '^[a-z]([a-z0-9]|-[a-z0-9])*$'),
    -- The canonical UUID form is an id in a path, never a slug (#12).
    CONSTRAINT products_slug_not_uuid_check CHECK (
        slug !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    CONSTRAINT products_version_check CHECK (version >= 1),
    CONSTRAINT products_color_check CHECK (color IS NULL OR color ~ '^#[0-9a-fA-F]{6}$'),
    CONSTRAINT products_icon_check CHECK (icon IS NULL OR char_length(icon) <= 8)
);

CREATE TABLE product_teams (
    org_id     uuid        NOT NULL,
    product_id uuid        NOT NULL,
    team_id    uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT product_teams_pkey PRIMARY KEY (product_id, team_id),
    CONSTRAINT product_teams_product_fk FOREIGN KEY (org_id, product_id)
        REFERENCES products (org_id, id) ON DELETE CASCADE,
    CONSTRAINT product_teams_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE
);
CREATE INDEX product_teams_team_idx ON product_teams (org_id, team_id);
