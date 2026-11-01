CREATE INDEX IF NOT EXISTS approvals_tenant_status_idx ON approvals (tenant_id, status, approval_id);
CREATE INDEX IF NOT EXISTS approvals_run_idx ON approvals (run_id, approval_id);
