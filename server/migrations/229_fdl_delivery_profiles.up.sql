CREATE TABLE fdl_delivery_profile (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    squad_id UUID NOT NULL,
    controller_config JSONB NOT NULL,
    role_bindings JSONB NOT NULL,
    created_by UUID NOT NULL,
    archived_at TIMESTAMPTZ,
    archived_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (jsonb_typeof(controller_config) = 'object'),
    CHECK (jsonb_typeof(role_bindings) = 'array')
);

ALTER TABLE issue
    ADD COLUMN orchestration_mode TEXT NOT NULL DEFAULT 'squad'
        CHECK (orchestration_mode IN ('squad', 'fdl')),
    ADD COLUMN fdl_run_id TEXT;

CREATE TABLE fdl_issue_run (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    profile_id UUID NOT NULL,
    fdl_run_id TEXT,
    status TEXT NOT NULL DEFAULT 'pending_executor'
        CHECK (status IN ('pending_executor', 'initializing', 'running',
                         'awaiting_human_decision', 'recovering',
                         'handoff_ready', 'completed', 'failed', 'cancelled')),
    phase TEXT NOT NULL DEFAULT 'setup',
    profile_snapshot JSONB NOT NULL,
    worktree_snapshot JSONB NOT NULL,
    runtime_snapshot JSONB NOT NULL,
    state_projection JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    CHECK (jsonb_typeof(profile_snapshot) = 'object'),
    CHECK (jsonb_typeof(worktree_snapshot) = 'object'),
    CHECK (jsonb_typeof(runtime_snapshot) = 'object'),
    CHECK (jsonb_typeof(state_projection) = 'object')
);
