CREATE TABLE fdl_human_decision (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    fdl_issue_run_id UUID NOT NULL,
    action_id TEXT NOT NULL,
    decision TEXT NOT NULL,
    submitted_by UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'submitted')),
    operation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    submitted_at TIMESTAMPTZ
);
