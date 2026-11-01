CREATE TABLE IF NOT EXISTS session_participants (
    session_id   uuid        NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    tenant_id    uuid        NOT NULL,
    principal_id uuid        NOT NULL,
    role         text        NOT NULL CHECK (role IN ('owner', 'member', 'viewer')),
    joined_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, principal_id)
);
CREATE INDEX IF NOT EXISTS session_participants_principal_idx ON session_participants (tenant_id, principal_id);

INSERT INTO session_participants (session_id, tenant_id, principal_id, role, joined_at)
SELECT session_id, tenant_id, created_by, 'owner', created_at FROM sessions
ON CONFLICT DO NOTHING;
