CREATE UNIQUE INDEX CONCURRENTLY idx_code_worktree_workspace_daemon_path ON code_worktree(workspace_id, daemon_id, canonical_path);
