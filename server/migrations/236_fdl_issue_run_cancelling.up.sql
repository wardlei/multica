ALTER TABLE fdl_issue_run
    DROP CONSTRAINT IF EXISTS fdl_issue_run_status_check;

ALTER TABLE fdl_issue_run
    ADD CONSTRAINT fdl_issue_run_status_check
    CHECK (status IN ('pending_executor', 'initializing', 'running', 'awaiting_human_decision',
                     'recovering', 'handoff_ready', 'cancelling', 'completed', 'failed', 'cancelled'));
