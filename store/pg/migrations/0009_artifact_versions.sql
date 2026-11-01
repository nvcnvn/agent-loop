ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS version integer NOT NULL DEFAULT 1;

UPDATE artifacts a SET version = v.n
FROM (SELECT artifact_id, row_number() OVER (PARTITION BY session_id, name ORDER BY created_at, artifact_id) AS n
      FROM artifacts) v
WHERE a.artifact_id = v.artifact_id AND v.n > 1;

CREATE UNIQUE INDEX IF NOT EXISTS artifacts_version_idx ON artifacts (session_id, name, version);
