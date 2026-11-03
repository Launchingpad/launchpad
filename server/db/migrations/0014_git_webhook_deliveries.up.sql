-- LAUN-87 (T17, decisions-T17 #9): migration B. The deliveries ring: one
-- summary row per webhook request, newest 100 per integration. It holds no
-- payload field (no title, URL, branch or user), no secret, no signature, no
-- caller IP and no User-Agent. `event` is the provider's own event name
-- (checked and cut in the app), `error_code` is one of our codes.
--
-- A request that could not be tied to one integration (a signature that
-- matched none) has a NULL integration_id: the composite FK tolerates NULL
-- (MATCH SIMPLE). Those rows are trimmed per org and provider.
CREATE TABLE git_webhook_deliveries (
    id             uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id         uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    integration_id uuid,
    provider       text        NOT NULL
                   CONSTRAINT git_webhook_deliveries_provider_check CHECK (provider IN ('gitlab', 'github', 'bitbucket')),
    received_at    timestamptz NOT NULL DEFAULT now(),
    result         text        NOT NULL
                   CONSTRAINT git_webhook_deliveries_result_check CHECK (result IN
                       ('processed', 'ignored', 'duplicate', 'unmapped', 'bad_signature', 'bad_request', 'error')),
    event          text        NOT NULL DEFAULT ''
                   CONSTRAINT git_webhook_deliveries_event_check CHECK (char_length(event) <= 64),
    repo_key       text
                   CONSTRAINT git_webhook_deliveries_repo_key_check CHECK (repo_key IS NULL OR char_length(repo_key) <= 200),
    http_status    integer     NOT NULL
                   CONSTRAINT git_webhook_deliveries_status_check CHECK (http_status BETWEEN 100 AND 599),
    error_code     text
                   CONSTRAINT git_webhook_deliveries_error_code_check CHECK (error_code IS NULL OR char_length(error_code) <= 64),
    CONSTRAINT git_webhook_deliveries_integration_fk FOREIGN KEY (org_id, integration_id)
        REFERENCES git_integrations (org_id, id) ON DELETE CASCADE
);

-- The list route, the last-delivery summary and the trim read newest first.
CREATE INDEX git_webhook_deliveries_integration_idx
    ON git_webhook_deliveries (org_id, integration_id, received_at DESC, id DESC);
-- The NULL-integration rows (trimmed and listed per org and provider).
CREATE INDEX git_webhook_deliveries_unbound_idx
    ON git_webhook_deliveries (org_id, provider, received_at DESC, id DESC)
    WHERE integration_id IS NULL;
