CREATE UNIQUE INDEX CONCURRENTLY fdl_human_decision_run_action_idx
    ON fdl_human_decision (fdl_issue_run_id, action_id);
