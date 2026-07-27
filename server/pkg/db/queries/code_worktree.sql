-- name: ListCodeWorktrees :many
SELECT * FROM code_worktree WHERE workspace_id = $1 ORDER BY updated_at DESC, created_at DESC;
-- name: GetCodeWorktreeInWorkspace :one
SELECT * FROM code_worktree WHERE id = $1 AND workspace_id = $2;
-- name: GetProjectCodeWorktree :one
SELECT cw.* FROM code_worktree cw JOIN project p ON p.default_code_worktree_id = cw.id WHERE p.id = $1 AND p.workspace_id = $2 AND cw.workspace_id = $2;
-- name: CreateCodeWorktree :one
INSERT INTO code_worktree (workspace_id, daemon_id, local_path, canonical_path, repository_url, branch, head_sha, is_dirty, inspected_at, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now(),$9) RETURNING *;
-- name: UpdateCodeWorktreeFromInspection :one
UPDATE code_worktree SET local_path=$3,canonical_path=$4,repository_url=$5,branch=$6,head_sha=$7,is_dirty=$8,inspected_at=now(),updated_at=now() WHERE id=$1 AND workspace_id=$2 AND updated_at=$9 RETURNING *;
-- name: LockCodeWorktreeInWorkspace :one
SELECT * FROM code_worktree WHERE id=$1 AND workspace_id=$2 FOR UPDATE;
-- name: DeleteCodeWorktreeInWorkspace :exec
DELETE FROM code_worktree WHERE id=$1 AND workspace_id=$2;
-- name: CountActiveTasksUsingCodeWorktree :one
SELECT count(*) FROM agent_task_queue WHERE status IN ('queued','dispatched','running','waiting_local_directory','deferred') AND worktree_context ->> 'worktree_id' = sqlc.arg(worktree_id)::text;
-- name: CreateCodeWorktreeInspection :one
INSERT INTO code_worktree_inspection (workspace_id,daemon_id,local_path,worktree_id,expected_updated_at,status,expires_at,created_by) VALUES ($1,$2,$3,$4,$5,'pending',$6,$7) RETURNING *;
-- name: GetCodeWorktreeInspectionInWorkspace :one
SELECT * FROM code_worktree_inspection WHERE id=$1 AND workspace_id=$2;
-- name: CompleteCodeWorktreeInspection :one
UPDATE code_worktree_inspection SET status=CASE WHEN $8::text='' THEN 'completed' ELSE 'failed' END,canonical_path=$3,repository_url=$4,branch=$5,head_sha=$6,is_dirty=$7,error=NULLIF($8::text,''),completed_at=now() WHERE id=$1 AND workspace_id=$2 AND daemon_id=$9 AND status='pending' AND expires_at>now() RETURNING *;
-- name: ConsumeCodeWorktreeInspection :one
UPDATE code_worktree_inspection SET status='consumed' WHERE id=$1 AND workspace_id=$2 AND status='completed' RETURNING *;
