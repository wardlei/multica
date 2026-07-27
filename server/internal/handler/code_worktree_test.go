package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func createCodeWorktreeFixture(t *testing.T, daemonID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	w := httptest.NewRecorder()
	testHandler.CreateProject(w, newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Code worktree fixture",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateProject: %d %s", w.Code, w.Body.String())
	}
	var project ProjectResponse
	if err := json.NewDecoder(w.Body).Decode(&project); err != nil {
		t.Fatalf("decode project: %v", err)
	}

	var worktreeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO code_worktree (
			workspace_id, daemon_id, local_path, canonical_path, repository_url,
			branch, head_sha, is_dirty, inspected_at, created_by
		) VALUES ($1, $2, $3, $3, $4, 'main', $5, false, now(), $6)
		RETURNING id
	`, testWorkspaceID, daemonID, "/tmp/multica-worktree-"+daemonID,
		"github.com/multica-ai/multica", "0123456789012345678901234567890123456789", testUserID).Scan(&worktreeID); err != nil {
		t.Fatalf("create code worktree: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE worktree_context ->> 'worktree_id' = $1`, worktreeID)
		testPool.Exec(context.Background(), `DELETE FROM project_resource WHERE project_id = $1`, project.ID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, project.ID)
		testPool.Exec(context.Background(), `DELETE FROM code_worktree WHERE id = $1`, worktreeID)
	})
	return project.ID, worktreeID
}

func TestCodeWorktreeBindingRejectsLocalDirectoryOnSameDaemon(t *testing.T) {
	projectID, worktreeID := createCodeWorktreeFixture(t, "worktree-test-daemon")

	w := httptest.NewRecorder()
	req := withURLParam(newRequest("PUT", "/api/projects/"+projectID+"/code-worktree", map[string]any{
		"worktree_id": worktreeID,
	}), "id", projectID)
	testHandler.SetProjectCodeWorktree(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("SetProjectCodeWorktree: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = withURLParam(newRequest("POST", "/api/projects/"+projectID+"/resources", map[string]any{
		"resource_type": "local_directory",
		"resource_ref": map[string]any{
			"local_path": "/tmp/multica-legacy-directory",
			"daemon_id":  "worktree-test-daemon",
		},
	}), "id", projectID)
	testHandler.CreateProjectResource(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("CreateProjectResource with matching daemon: got %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestDeleteCodeWorktreeRejectsActiveTaskSnapshot(t *testing.T) {
	_, worktreeID := createCodeWorktreeFixture(t, "worktree-task-daemon")
	ctx := context.Background()
	var agentID, taskID string
	if err := testPool.QueryRow(ctx, `SELECT id FROM agent WHERE workspace_id = $1 AND runtime_id = $2 LIMIT 1`, testWorkspaceID, testRuntimeID).Scan(&agentID); err != nil {
		t.Fatalf("find fixture agent: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, worktree_context)
		VALUES ($1, $2, 'queued', 1, jsonb_build_object('worktree_id', $3::text))
		RETURNING id
	`, agentID, testRuntimeID, worktreeID).Scan(&taskID); err != nil {
		t.Fatalf("create task snapshot: %v", err)
	}

	w := httptest.NewRecorder()
	req := withURLParam(newRequest("DELETE", "/api/code-worktrees/"+worktreeID, nil), "id", worktreeID)
	testHandler.DeleteCodeWorktree(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("DeleteCodeWorktree with active task: got %d, want 409: %s", w.Code, w.Body.String())
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status = 'cancelled' WHERE id = $1`, taskID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}

	w = httptest.NewRecorder()
	req = withURLParam(newRequest("DELETE", "/api/code-worktrees/"+worktreeID, nil), "id", worktreeID)
	testHandler.DeleteCodeWorktree(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteCodeWorktree after task cancellation: got %d, want 204: %s", w.Code, w.Body.String())
	}
}
