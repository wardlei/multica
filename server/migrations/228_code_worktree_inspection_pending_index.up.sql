CREATE INDEX CONCURRENTLY idx_code_worktree_inspection_pending ON code_worktree_inspection(workspace_id, daemon_id, expires_at) WHERE status = 'pending';
