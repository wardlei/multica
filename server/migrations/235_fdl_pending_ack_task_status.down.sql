ALTER TABLE agent_task_queue DROP CONSTRAINT IF EXISTS agent_task_queue_status_check;

ALTER TABLE agent_task_queue ADD CONSTRAINT agent_task_queue_status_check
    CHECK (status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'completed', 'failed', 'cancelled'));
