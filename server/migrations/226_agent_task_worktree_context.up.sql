ALTER TABLE agent_task_queue ADD COLUMN worktree_context JSONB NOT NULL DEFAULT '{}'::jsonb;
