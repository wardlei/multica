ALTER TABLE fdl_issue_run
    ADD COLUMN issue_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(issue_snapshot) = 'object');
