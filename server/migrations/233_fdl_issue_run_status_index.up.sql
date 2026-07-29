CREATE INDEX CONCURRENTLY idx_fdl_issue_run_workspace_status
    ON fdl_issue_run(workspace_id, status, updated_at DESC);
