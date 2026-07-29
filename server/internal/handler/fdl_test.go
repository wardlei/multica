package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCreateFDLDeliveryIssueFreezesProfileAndSkipsSquadLeader(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	ctx := context.Background()
	fixture := createFDLDeliveryFixture(t, ctx)

	profileBody := map[string]any{
		"name":              "FDL freeze profile",
		"description":       "A delivery workflow for a tested worktree.",
		"squad_id":          fixture.squadID,
		"controller_config": fdlTestControllerConfig(),
		"role_bindings":     fixture.roleBindings,
	}
	profileRequest := newRequest(http.MethodPost, "/api/fdl-delivery-profiles?workspace_id="+testWorkspaceID, profileBody)
	profileRecorder := httptest.NewRecorder()
	testHandler.CreateFDLDeliveryProfile(profileRecorder, profileRequest)
	if profileRecorder.Code != http.StatusCreated {
		t.Fatalf("CreateFDLDeliveryProfile: expected 201, got %d: %s", profileRecorder.Code, profileRecorder.Body.String())
	}
	var profile FDLDeliveryProfileResponse
	if err := json.NewDecoder(profileRecorder.Body).Decode(&profile); err != nil {
		t.Fatalf("decode profile: %v", err)
	}

	issueRequest := newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":          "FDL issue freezes every executor input",
		"status":         "todo",
		"project_id":     fixture.projectID,
		"fdl_profile_id": profile.ID,
	})
	issueRecorder := httptest.NewRecorder()
	testHandler.CreateIssue(issueRecorder, issueRequest)
	if issueRecorder.Code != http.StatusCreated {
		t.Fatalf("CreateIssue FDL: expected 201, got %d: %s", issueRecorder.Code, issueRecorder.Body.String())
	}
	var issue IssueResponse
	if err := json.NewDecoder(issueRecorder.Body).Decode(&issue); err != nil {
		t.Fatalf("decode FDL issue: %v", err)
	}
	if issue.OrchestrationMode != "fdl" {
		t.Fatalf("orchestration_mode = %q, want fdl", issue.OrchestrationMode)
	}
	if issue.FDLRunID != nil {
		t.Fatalf("fdl_run_id = %q before executor initialization, want nil", *issue.FDLRunID)
	}
	if issue.AssigneeType == nil || *issue.AssigneeType != "squad" || issue.AssigneeID == nil || *issue.AssigneeID != fixture.squadID {
		t.Fatalf("FDL issue assignee = (%v, %v), want profile squad %s", issue.AssigneeType, issue.AssigneeID, fixture.squadID)
	}

	// The Issue route exposes only the UI/audit projection, never the frozen
	// absolute worktree path, Controller config, result locations, or tokens.
	runRequest := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issue.ID+"/fdl-run?workspace_id="+testWorkspaceID, nil), "id", issue.ID)
	runRecorder := httptest.NewRecorder()
	testHandler.GetFDLIssueRun(runRecorder, runRequest)
	if runRecorder.Code != http.StatusOK {
		t.Fatalf("GetFDLIssueRun: expected 200, got %d: %s", runRecorder.Code, runRecorder.Body.String())
	}
	var run FDLIssueRunResponse
	if err := json.NewDecoder(runRecorder.Body).Decode(&run); err != nil {
		t.Fatalf("decode FDL issue run: %v", err)
	}
	if run.IssueID != issue.ID || run.Status != "pending_executor" || run.Phase != "setup" {
		t.Fatalf("unexpected FDL Issue run projection: %#v", run)
	}
	if body := runRecorder.Body.String(); containsAny(body, "/tmp/fdl-test-repo", "controller_config", "submission_token") {
		t.Fatalf("FDL Issue run projection leaked private launch state: %s", body)
	}

	var taskCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issue.ID).Scan(&taskCount); err != nil {
		t.Fatalf("count leader tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("FDL issue enqueued %d legacy task(s), want 0", taskCount)
	}

	var profileSnapshot, worktreeSnapshot, runtimeSnapshot []byte
	if err := testPool.QueryRow(ctx, `
		SELECT profile_snapshot, worktree_snapshot, runtime_snapshot
		FROM fdl_issue_run WHERE issue_id = $1
	`, issue.ID).Scan(&profileSnapshot, &worktreeSnapshot, &runtimeSnapshot); err != nil {
		t.Fatalf("load FDL issue run: %v", err)
	}
	for name, snapshot := range map[string][]byte{
		"profile": profileSnapshot, "worktree": worktreeSnapshot, "runtime": runtimeSnapshot,
	} {
		var decoded map[string]any
		if err := json.Unmarshal(snapshot, &decoded); err != nil {
			t.Fatalf("decode %s snapshot: %v", name, err)
		}
		if len(decoded) == 0 {
			t.Fatalf("%s snapshot is empty", name)
		}
	}

	// Template changes affect only future Issues. The active run keeps the
	// complete launch-time profile snapshot even if the profile is later
	// updated or archived.
	updateRecorder := httptest.NewRecorder()
	testHandler.UpdateFDLDeliveryProfile(updateRecorder, withURLParam(newRequest(http.MethodPut, "/api/fdl-delivery-profiles/"+profile.ID+"?workspace_id="+testWorkspaceID, map[string]any{
		"description": "Changed after the FDL Issue was created",
	}), "id", profile.ID))
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("UpdateFDLDeliveryProfile: expected 200, got %d: %s", updateRecorder.Code, updateRecorder.Body.String())
	}
	var frozenProfile map[string]any
	if err := json.Unmarshal(profileSnapshot, &frozenProfile); err != nil {
		t.Fatalf("decode frozen profile snapshot: %v", err)
	}
	if frozenProfile["profile_description"] != profileBody["description"] {
		t.Fatalf("profile snapshot description = %#v, want original %#v", frozenProfile["profile_description"], profileBody["description"])
	}

	archiveRecorder := httptest.NewRecorder()
	testHandler.ArchiveFDLDeliveryProfile(archiveRecorder, withURLParam(newRequest(http.MethodDelete, "/api/fdl-delivery-profiles/"+profile.ID+"?workspace_id="+testWorkspaceID, nil), "id", profile.ID))
	if archiveRecorder.Code != http.StatusNoContent {
		t.Fatalf("ArchiveFDLDeliveryProfile: expected 204, got %d: %s", archiveRecorder.Code, archiveRecorder.Body.String())
	}
	postArchiveRunRecorder := httptest.NewRecorder()
	testHandler.GetFDLIssueRun(postArchiveRunRecorder, runRequest)
	if postArchiveRunRecorder.Code != http.StatusOK {
		t.Fatalf("GetFDLIssueRun after profile archive: expected 200, got %d: %s", postArchiveRunRecorder.Code, postArchiveRunRecorder.Body.String())
	}
}

func TestCreateFDLDeliveryIssueRejectsRoleWithoutInvocationPermission(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	ctx := context.Background()
	fixture := createFDLDeliveryFixture(t, ctx)

	// This squad remains manageable by the test user, but one frozen role is
	// deliberately owned by a different user and has no invocation grant.
	var otherUserID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
	`, "FDL inaccessible role owner", "fdl-inaccessible-"+uuid.NewString()+"@multica.test").Scan(&otherUserID); err != nil {
		t.Fatalf("create inaccessible FDL role owner: %v", err)
	}
	roleAgentID := fixture.roleBindings[0]["agent_id"]
	if _, err := testPool.Exec(ctx, `UPDATE agent SET owner_id = $2 WHERE id = $1`, roleAgentID, otherUserID); err != nil {
		t.Fatalf("make FDL role non-invocable: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `UPDATE agent SET owner_id = $2 WHERE id = $1`, roleAgentID, testUserID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, otherUserID)
	})
	profileRequest := newRequest(http.MethodPost, "/api/fdl-delivery-profiles?workspace_id="+testWorkspaceID, map[string]any{
		"name": "FDL permission profile", "squad_id": fixture.squadID,
		"controller_config": fdlTestControllerConfig(), "role_bindings": fixture.roleBindings,
	})
	profileRecorder := httptest.NewRecorder()
	testHandler.CreateFDLDeliveryProfile(profileRecorder, profileRequest)
	if profileRecorder.Code != http.StatusCreated {
		t.Fatalf("CreateFDLDeliveryProfile: expected 201, got %d: %s", profileRecorder.Code, profileRecorder.Body.String())
	}
	var profile FDLDeliveryProfileResponse
	if err := json.NewDecoder(profileRecorder.Body).Decode(&profile); err != nil {
		t.Fatalf("decode profile: %v", err)
	}

	issueRecorder := httptest.NewRecorder()
	testHandler.CreateIssue(issueRecorder, newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "FDL issue must not bypass role permission", "project_id": fixture.projectID, "fdl_profile_id": profile.ID,
	}))
	if issueRecorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("CreateIssue FDL: expected 422 for inaccessible role, got %d: %s", issueRecorder.Code, issueRecorder.Body.String())
	}
}

func TestFDLIssueNeverUsesLegacyIssueOrCommentTriggers(t *testing.T) {
	if testHandler == nil || testHandler.IssueService == nil {
		t.Skip("handler test fixture is unavailable")
	}
	issue := db.Issue{OrchestrationMode: "fdl"}
	if _, ok := testHandler.IssueService.WillEnqueueRun(context.Background(), service.IssueTriggerInput{
		Issue: issue, IsCreate: true,
	}, service.IssueTriggerProbe{}); ok {
		t.Fatal("FDL issue unexpectedly accepted a legacy assignment trigger")
	}
	triggers, targets := testHandler.computeCommentAgentTriggers(
		context.Background(), issue, "@agent:agent-id please continue", nil, "member", testUserID, commentTriggerComputeOptions{},
	)
	if len(triggers) != 0 || len(targets) != 0 {
		t.Fatalf("FDL comment produced legacy dispatches: triggers=%#v targets=%#v", triggers, targets)
	}
}

func TestDaemonFDLRunLifecycleIsWorktreeBoundAndSnapshotsIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	ctx := context.Background()
	fixture := createFDLDeliveryFixture(t, ctx)

	profileRecorder := httptest.NewRecorder()
	testHandler.CreateFDLDeliveryProfile(profileRecorder, newRequest(http.MethodPost, "/api/fdl-delivery-profiles?workspace_id="+testWorkspaceID, map[string]any{
		"name": "FDL daemon lifecycle profile", "squad_id": fixture.squadID,
		"controller_config": fdlTestControllerConfig(), "role_bindings": fixture.roleBindings,
	}))
	if profileRecorder.Code != http.StatusCreated {
		t.Fatalf("CreateFDLDeliveryProfile: expected 201, got %d: %s", profileRecorder.Code, profileRecorder.Body.String())
	}
	var profile FDLDeliveryProfileResponse
	if err := json.NewDecoder(profileRecorder.Body).Decode(&profile); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	description := "Freeze this delivery request before the daemon receives it."
	issueRecorder := httptest.NewRecorder()
	testHandler.CreateIssue(issueRecorder, newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "FDL daemon lifecycle", "description": description,
		"project_id": fixture.projectID, "fdl_profile_id": profile.ID,
	}))
	if issueRecorder.Code != http.StatusCreated {
		t.Fatalf("CreateIssue FDL: expected 201, got %d: %s", issueRecorder.Code, issueRecorder.Body.String())
	}
	var issue IssueResponse
	if err := json.NewDecoder(issueRecorder.Body).Decode(&issue); err != nil {
		t.Fatalf("decode issue: %v", err)
	}

	daemonContext := middleware.WithDaemonContext(ctx, testWorkspaceID, "fdl-delivery-test-daemon")
	listRequest := httptest.NewRequest(http.MethodGet, "/api/daemon/fdl-runs/pending", nil).WithContext(daemonContext)
	listRecorder := httptest.NewRecorder()
	testHandler.ListPendingFDLIssueRunsForDaemon(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("ListPendingFDLIssueRunsForDaemon: expected 200, got %d: %s", listRecorder.Code, listRecorder.Body.String())
	}
	var pending struct {
		Runs []daemonFDLIssueRunResponse `json:"runs"`
	}
	if err := json.NewDecoder(listRecorder.Body).Decode(&pending); err != nil {
		t.Fatalf("decode pending FDL runs: %v", err)
	}
	if len(pending.Runs) != 1 || pending.Runs[0].IssueID != issue.ID {
		t.Fatalf("pending FDL runs = %#v, want only issue %s", pending.Runs, issue.ID)
	}
	if !containsAny(string(pending.Runs[0].IssueSnapshot), description) {
		t.Fatalf("daemon launch payload omitted frozen Issue description: %s", pending.Runs[0].IssueSnapshot)
	}

	wrongDaemonRequest := httptest.NewRequest(http.MethodGet, "/api/daemon/fdl-runs/pending", nil).WithContext(middleware.WithDaemonContext(ctx, testWorkspaceID, "another-daemon"))
	wrongDaemonRecorder := httptest.NewRecorder()
	testHandler.ListPendingFDLIssueRunsForDaemon(wrongDaemonRecorder, wrongDaemonRequest)
	if wrongDaemonRecorder.Code != http.StatusOK || containsAny(wrongDaemonRecorder.Body.String(), issue.ID) {
		t.Fatalf("wrong daemon received FDL run: status=%d body=%s", wrongDaemonRecorder.Code, wrongDaemonRecorder.Body.String())
	}

	initializeRequest := withURLParam(httptest.NewRequest(http.MethodPost, "/api/daemon/fdl-runs/"+pending.Runs[0].ID+"/initialize", strings.NewReader(`{"fdl_run_id":"fdl-run-test-001"}`)).WithContext(daemonContext), "id", pending.Runs[0].ID)
	initializeRecorder := httptest.NewRecorder()
	testHandler.InitializeFDLIssueRunForDaemon(initializeRecorder, initializeRequest)
	if initializeRecorder.Code != http.StatusOK {
		t.Fatalf("InitializeFDLIssueRunForDaemon: expected 200, got %d: %s", initializeRecorder.Code, initializeRecorder.Body.String())
	}
	var issueRunID string
	if err := testPool.QueryRow(ctx, `SELECT fdl_run_id FROM issue WHERE id = $1`, issue.ID).Scan(&issueRunID); err != nil {
		t.Fatalf("read Issue FDL run ID: %v", err)
	}
	if issueRunID != "fdl-run-test-001" {
		t.Fatalf("Issue fdl_run_id = %q, want fdl-run-test-001", issueRunID)
	}

	projectionRequest := withURLParam(httptest.NewRequest(http.MethodPost, "/api/daemon/fdl-runs/"+pending.Runs[0].ID+"/projection", strings.NewReader(`{"status":"running","phase":"intake","summary":"Controller dispatched intake"}`)).WithContext(daemonContext), "id", pending.Runs[0].ID)
	projectionRecorder := httptest.NewRecorder()
	testHandler.UpdateFDLIssueRunProjectionForDaemon(projectionRecorder, projectionRequest)
	if projectionRecorder.Code != http.StatusOK {
		t.Fatalf("UpdateFDLIssueRunProjectionForDaemon: expected 200, got %d: %s", projectionRecorder.Code, projectionRecorder.Body.String())
	}

	directTaskRequest := withURLParam(httptest.NewRequest(http.MethodPost, "/api/daemon/fdl-runs/"+pending.Runs[0].ID+"/tasks", strings.NewReader(`{"role":"intake","instructions":"Prepare the frozen FDL intake report in your assigned result directory.","dispatch_key":"multica-dispatch-test-001"}`)).WithContext(daemonContext), "id", pending.Runs[0].ID)
	directTaskRecorder := httptest.NewRecorder()
	testHandler.CreateFDLAgentTaskForDaemon(directTaskRecorder, directTaskRequest)
	if directTaskRecorder.Code != http.StatusCreated {
		t.Fatalf("CreateFDLAgentTaskForDaemon: expected 201, got %d: %s", directTaskRecorder.Code, directTaskRecorder.Body.String())
	}
	var taskID string
	if err := json.NewDecoder(directTaskRecorder.Body).Decode(&struct {
		TaskID *string `json:"task_id"`
	}{TaskID: &taskID}); err != nil || taskID == "" {
		t.Fatalf("decode direct FDL task: id=%q err=%v", taskID, err)
	}
	var maxAttempts int
	var isLeader bool
	var taskRuntimeID string
	var taskContext, taskWorktreeContext []byte
	if err := testPool.QueryRow(ctx, `SELECT max_attempts, is_leader_task, runtime_id::text, context, worktree_context FROM agent_task_queue WHERE id = $1`, taskID).Scan(&maxAttempts, &isLeader, &taskRuntimeID, &taskContext, &taskWorktreeContext); err != nil {
		t.Fatalf("load direct FDL task: %v", err)
	}
	if maxAttempts != 1 || isLeader || taskRuntimeID == "" {
		t.Fatalf("direct FDL task has invalid retry/leader/runtime fields: attempts=%d leader=%v runtime=%s", maxAttempts, isLeader, taskRuntimeID)
	}
	var directContext struct {
		FDLDirect bool `json:"fdl_direct"`
	}
	if err := json.Unmarshal(taskContext, &directContext); err != nil || !directContext.FDLDirect {
		t.Fatalf("direct FDL task context did not retain fdl_direct marker: %s", taskContext)
	}
	var worktree service.WorktreeContext
	if err := json.Unmarshal(taskWorktreeContext, &worktree); err != nil {
		t.Fatalf("decode direct FDL task worktree context: %v", err)
	}
	if worktree.ExpectedBranch == "" || worktree.ExpectedHeadSHA == "" || worktree.RepositoryURL == "" || !worktree.MustBeClean {
		t.Fatalf("direct FDL task stored an invalid daemon worktree contract: %#v", worktree)
	}

	// An FDL cancellation differs deliberately from ordinary Squad Issue
	// cancellation: even a pre-ack direct task must be made terminal before
	// the daemon receives the Controller termination request.
	cancelRecorder := httptest.NewRecorder()
	cancelRequest := withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue.ID+"?workspace_id="+testWorkspaceID, map[string]any{
		"status": "cancelled",
	}), "id", issue.ID)
	testHandler.UpdateIssue(cancelRecorder, cancelRequest)
	if cancelRecorder.Code != http.StatusOK {
		t.Fatalf("cancel FDL issue: expected 200, got %d: %s", cancelRecorder.Code, cancelRecorder.Body.String())
	}
	var directStatus, runStatus, runPhase string
	if err := testPool.QueryRow(ctx, `
		SELECT t.status, r.status, r.phase
		FROM agent_task_queue t
		JOIN fdl_issue_run r ON r.issue_id = t.issue_id
		WHERE t.id = $1
	`, taskID).Scan(&directStatus, &runStatus, &runPhase); err != nil {
		t.Fatalf("load cancelled FDL task and run: %v", err)
	}
	if directStatus != "cancelled" || runStatus != "cancelling" || runPhase != "cancelled" {
		t.Fatalf("FDL cancellation state = task=%q run=%q/%q, want cancelled cancelling/cancelled", directStatus, runStatus, runPhase)
	}

	wrongProjectionRequest := withURLParam(httptest.NewRequest(http.MethodPost, "/api/daemon/fdl-runs/"+pending.Runs[0].ID+"/projection", strings.NewReader(`{"status":"running","phase":"intake"}`)).WithContext(middleware.WithDaemonContext(ctx, testWorkspaceID, "another-daemon")), "id", pending.Runs[0].ID)
	wrongProjectionRecorder := httptest.NewRecorder()
	testHandler.UpdateFDLIssueRunProjectionForDaemon(wrongProjectionRecorder, wrongProjectionRequest)
	if wrongProjectionRecorder.Code != http.StatusConflict {
		t.Fatalf("wrong daemon projection: expected 409, got %d: %s", wrongProjectionRecorder.Code, wrongProjectionRecorder.Body.String())
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

type fdlDeliveryFixture struct {
	projectID    string
	squadID      string
	roleBindings []map[string]string
	cleanupIDs   []string
}

func createFDLDeliveryFixture(t *testing.T, ctx context.Context) fdlDeliveryFixture {
	t.Helper()
	fixture := fdlDeliveryFixture{}
	const daemonID = "fdl-delivery-test-daemon"
	var runtimeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at)
		VALUES ($1, $2, 'FDL Test Runtime', 'local', 'codex', 'online', 'FDL test daemon', '{}'::jsonb, $3, now())
		RETURNING id
	`, testWorkspaceID, daemonID, testUserID).Scan(&runtimeID); err != nil {
		t.Fatalf("create FDL runtime: %v", err)
	}
	fixture.cleanupIDs = append(fixture.cleanupIDs, runtimeID)

	roles := []string{"intake", "planner", "implementer", "reviewer:correctness", "reviewer:regression", "reviewer:specialist", "explorer:impact_analysis"}
	agentIDs := make([]string, 0, len(roles))
	for _, role := range roles {
		var agentID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, permission_mode, max_concurrent_tasks, owner_id, model, thinking_level, service_tier)
			VALUES ($1, $2, '', 'local', '{}'::jsonb, $3, 'workspace', 'public_to', 1, $4, 'gpt-5.6-terra', 'high', 'priority')
			RETURNING id
		`, testWorkspaceID, "FDL "+role, runtimeID, testUserID).Scan(&agentID); err != nil {
			t.Fatalf("create %s agent: %v", role, err)
		}
		agentIDs = append(agentIDs, agentID)
		fixture.roleBindings = append(fixture.roleBindings, map[string]string{"role": role, "agent_id": agentID})
	}

	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, 'FDL Delivery Test Squad', '', $2, $3) RETURNING id
	`, testWorkspaceID, agentIDs[0], testUserID).Scan(&fixture.squadID); err != nil {
		t.Fatalf("create FDL squad: %v", err)
	}
	for _, agentID := range agentIDs {
		if _, err := testPool.Exec(ctx, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'worker')`, fixture.squadID, agentID); err != nil {
			t.Fatalf("add FDL squad member: %v", err)
		}
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, status) VALUES ($1, 'FDL Test Project', 'in_progress') RETURNING id
	`, testWorkspaceID).Scan(&fixture.projectID); err != nil {
		t.Fatalf("create FDL project: %v", err)
	}
	var worktreeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO code_worktree (workspace_id, daemon_id, local_path, canonical_path, repository_url, branch, head_sha, is_dirty, inspected_at, created_by)
		VALUES ($1, $2, '/tmp/fdl-test-repo', '/tmp/fdl-test-repo', 'github.com/example/fdl-test', 'main', repeat('a', 40), false, now(), $3)
		RETURNING id
	`, testWorkspaceID, daemonID, testUserID).Scan(&worktreeID); err != nil {
		t.Fatalf("create FDL worktree: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE project SET default_code_worktree_id = $2 WHERE id = $1`, fixture.projectID, worktreeID); err != nil {
		t.Fatalf("bind FDL worktree: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM fdl_issue_run WHERE workspace_id = $1`, testWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM fdl_delivery_profile WHERE workspace_id = $1`, testWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM code_worktree WHERE id = $1`, worktreeID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, fixture.projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM squad_member WHERE squad_id = $1`, fixture.squadID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, fixture.squadID)
		for _, agentID := range agentIDs {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
		}
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})
	return fixture
}

func fdlTestControllerConfig() map[string]any {
	return map[string]any{
		"schema_version": 4,
		"change_rules":   []any{}, "control_files": []any{}, "workspace_policy": map[string]any{},
		"capability_profiles": map[string]any{}, "gates": []any{}, "workflow_policy": map[string]any{},
		"contract_test_map": map[string]any{}, "known_failure_catalog": map[string]any{},
	}
}
