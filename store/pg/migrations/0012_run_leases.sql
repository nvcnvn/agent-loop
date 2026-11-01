ALTER TABLE runs ADD COLUMN IF NOT EXISTS owner text;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS lease_until timestamptz;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cancel_requested boolean NOT NULL DEFAULT false;
