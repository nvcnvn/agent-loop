CREATE TABLE IF NOT EXISTS agents (
    tenant_id  uuid        NOT NULL,
    name       text        NOT NULL,
    definition jsonb       NOT NULL,
    version    integer     NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, name)
);
