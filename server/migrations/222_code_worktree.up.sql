CREATE TABLE code_worktree (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id   UUID NOT NULL,
    daemon_id      TEXT NOT NULL,
    local_path     TEXT NOT NULL,
    canonical_path TEXT NOT NULL,
    repository_url TEXT NOT NULL,
    branch         TEXT NOT NULL,
    head_sha       TEXT NOT NULL,
    is_dirty       BOOLEAN NOT NULL DEFAULT FALSE,
    inspected_at   TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by     UUID
);
