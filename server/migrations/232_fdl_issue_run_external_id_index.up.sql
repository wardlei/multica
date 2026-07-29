CREATE UNIQUE INDEX CONCURRENTLY idx_fdl_issue_run_fdl_run
    ON fdl_issue_run(fdl_run_id)
    WHERE fdl_run_id IS NOT NULL;
