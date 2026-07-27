CREATE TABLE code_worktree_inspection (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id UUID NOT NULL, daemon_id TEXT NOT NULL, local_path TEXT NOT NULL,
    worktree_id UUID, expected_updated_at TIMESTAMPTZ, status TEXT NOT NULL CHECK (status IN ('pending', 'completed', 'expired', 'consumed', 'failed')),
    canonical_path TEXT, repository_url TEXT, branch TEXT, head_sha TEXT, is_dirty BOOLEAN, error TEXT,
    expires_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), completed_at TIMESTAMPTZ, created_by UUID
);
