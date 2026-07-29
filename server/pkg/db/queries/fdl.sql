-- name: CreateFDLDeliveryProfile :one
INSERT INTO fdl_delivery_profile (
    workspace_id, name, description, squad_id, controller_config, role_bindings, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetFDLDeliveryProfileInWorkspace :one
SELECT * FROM fdl_delivery_profile
WHERE id = $1 AND workspace_id = $2 AND archived_at IS NULL;

-- name: ListFDLDeliveryProfiles :many
SELECT * FROM fdl_delivery_profile
WHERE workspace_id = $1 AND archived_at IS NULL
ORDER BY name ASC;

-- name: UpdateFDLDeliveryProfile :one
UPDATE fdl_delivery_profile
SET name = COALESCE(sqlc.narg('name'), name),
    description = COALESCE(sqlc.narg('description'), description),
    squad_id = COALESCE(sqlc.narg('squad_id'), squad_id),
    controller_config = COALESCE(sqlc.narg('controller_config'), controller_config),
    role_bindings = COALESCE(sqlc.narg('role_bindings'), role_bindings),
    updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id') AND archived_at IS NULL
RETURNING *;

-- name: ArchiveFDLDeliveryProfile :one
UPDATE fdl_delivery_profile
SET archived_at = now(), archived_by = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND archived_at IS NULL
RETURNING *;

-- name: CreateFDLIssueRun :one
INSERT INTO fdl_issue_run (
    workspace_id, issue_id, profile_id, status, phase,
    profile_snapshot, worktree_snapshot, runtime_snapshot, issue_snapshot, state_projection, created_by
) VALUES ($1, $2, $3, 'pending_executor', 'setup', $4, $5, $6, $7, '{}'::jsonb, $8)
RETURNING *;

-- name: EnableIssueFDL :one
UPDATE issue
SET orchestration_mode = 'fdl', updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND orchestration_mode = 'squad'
RETURNING *;

-- name: GetFDLIssueRunByIssue :one
SELECT * FROM fdl_issue_run
WHERE issue_id = $1 AND workspace_id = $2;

-- name: GetFDLIssueRun :one
SELECT * FROM fdl_issue_run
WHERE id = $1 AND workspace_id = $2;

-- name: GetFDLIssueRunForDaemon :one
SELECT * FROM fdl_issue_run
WHERE id = $1
  AND workspace_id = $2
  AND worktree_snapshot @> jsonb_build_object('daemon_id', sqlc.arg(daemon_id)::text)::jsonb;

-- name: SetFDLIssueRunExternalID :one
UPDATE fdl_issue_run
SET fdl_run_id = $3, status = 'running', started_at = COALESCE(started_at, now()), updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND fdl_run_id IS NULL
RETURNING *;

-- name: ListPendingFDLIssueRunsForDaemon :many
SELECT * FROM fdl_issue_run
WHERE workspace_id = $1
  AND status = 'pending_executor'
  AND worktree_snapshot @> jsonb_build_object('daemon_id', sqlc.arg(daemon_id)::text)::jsonb
ORDER BY created_at ASC;

-- name: ListActiveFDLIssueRunsForDaemon :many
SELECT * FROM fdl_issue_run
WHERE workspace_id = $1
  AND status IN ('running', 'awaiting_human_decision', 'recovering', 'handoff_ready', 'cancelling')
  AND worktree_snapshot @> jsonb_build_object('daemon_id', sqlc.arg(daemon_id)::text)::jsonb
ORDER BY updated_at ASC;

-- name: InitializeFDLIssueRunForDaemon :one
WITH updated_run AS (
    UPDATE fdl_issue_run AS f
    SET fdl_run_id = $3,
        status = 'running',
        started_at = COALESCE(started_at, now()),
        updated_at = now()
    WHERE f.id = $1
      AND f.workspace_id = $2
      AND f.fdl_run_id IS NULL
      AND f.status = 'pending_executor'
      AND f.worktree_snapshot @> jsonb_build_object('daemon_id', sqlc.arg(daemon_id)::text)::jsonb
    RETURNING f.*
), updated_issue AS (
    UPDATE issue
    SET fdl_run_id = $3, updated_at = now()
    WHERE id = (SELECT issue_id FROM updated_run)
      AND workspace_id = $2
)
SELECT * FROM updated_run;

-- name: UpdateFDLIssueRunProjection :one
UPDATE fdl_issue_run
SET status = $3,
    phase = $4,
    state_projection = $5,
    finished_at = CASE WHEN $3 IN ('completed', 'failed', 'cancelled') THEN COALESCE(finished_at, now()) ELSE NULL END,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: RequestFDLIssueRunCancellation :one
UPDATE fdl_issue_run
SET status = 'cancelling',
    phase = 'cancelled',
    state_projection = jsonb_build_object(
        'status', 'cancelling',
        'phase', 'cancelled',
        'summary', sqlc.arg(reason)::text,
        'action_type', 'terminate_run'
    ),
    updated_at = now()
WHERE issue_id = $1
  AND workspace_id = $2
  AND status IN ('pending_executor', 'initializing', 'running', 'awaiting_human_decision', 'recovering', 'handoff_ready')
RETURNING *;

-- name: UpdateFDLIssueRunProjectionForDaemon :one
UPDATE fdl_issue_run
SET status = $3,
    phase = $4,
    state_projection = $5,
    finished_at = CASE WHEN $3 IN ('completed', 'failed', 'cancelled') THEN COALESCE(finished_at, now()) ELSE NULL END,
    updated_at = now()
WHERE id = $1
  AND workspace_id = $2
  AND worktree_snapshot @> jsonb_build_object('daemon_id', sqlc.arg(daemon_id)::text)::jsonb
  AND (status NOT IN ('completed', 'failed', 'cancelled') OR status = $3)
RETURNING *;
