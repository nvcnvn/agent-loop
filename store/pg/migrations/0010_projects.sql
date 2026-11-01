ALTER TABLE sessions ADD COLUMN IF NOT EXISTS project text NOT NULL DEFAULT '';

ALTER TABLE memories ADD COLUMN IF NOT EXISTS project text;
ALTER TABLE memories DROP CONSTRAINT IF EXISTS memories_scope_check;
ALTER TABLE memories ADD CONSTRAINT memories_scope_check CHECK (scope IN ('tenant', 'project', 'workflow', 'session'));
ALTER TABLE memories DROP CONSTRAINT IF EXISTS memories_project_check;
ALTER TABLE memories ADD CONSTRAINT memories_project_check CHECK (scope <> 'project' OR coalesce(project, '') <> '');
