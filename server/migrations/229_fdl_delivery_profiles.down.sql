ALTER TABLE issue
    DROP COLUMN IF EXISTS fdl_run_id,
    DROP COLUMN IF EXISTS orchestration_mode;

DROP TABLE IF EXISTS fdl_issue_run;
DROP TABLE IF EXISTS fdl_delivery_profile;
