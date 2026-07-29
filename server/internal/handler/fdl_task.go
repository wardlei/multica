package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fdlDirectTaskRequest struct {
	Role               string          `json:"role"`
	Instructions       string          `json:"instructions"`
	DispatchKey        string          `json:"dispatch_key"`
	Priority           int32           `json:"priority,omitempty"`
	ExecutionWorkspace json.RawMessage `json:"execution_workspace,omitempty"`
}

type fdlDaemonWorkspace struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	DaemonID      string `json:"daemon_id"`
	LocalPath     string `json:"local_path"`
	CanonicalPath string `json:"canonical_path"`
	ReadOnly      bool   `json:"read_only"`
}

// CreateFDLAgentTaskForDaemon is the only FDL task ingress. The daemon sends
// no Controller token or external-work identity; those bindings stay private
// in its local executor mailbox/mapping. The server accepts only a role frozen
// into this run and creates a one-attempt, non-leader task at its exact pin.
func (h *Handler) CreateFDLAgentTaskForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_issue_run_id")
	if !ok {
		return
	}
	var request fdlDirectTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	request.Role = strings.TrimSpace(request.Role)
	request.Instructions = strings.TrimSpace(request.Instructions)
	request.DispatchKey = strings.TrimSpace(request.DispatchKey)
	if request.Role == "" || request.Instructions == "" || len(request.Instructions) > 20000 || len(request.DispatchKey) < 16 || len(request.DispatchKey) > 128 || request.Priority < 0 || request.Priority > 3 {
		writeError(w, http.StatusBadRequest, "valid FDL role and instructions are required")
		return
	}
	run, err := h.Queries.GetFDLIssueRunForDaemon(r.Context(), db.GetFDLIssueRunForDaemonParams{ID: runID, WorkspaceID: workspaceID, DaemonID: daemonID})
	if err != nil || run.Status != "running" || !run.FdlRunID.Valid {
		writeError(w, http.StatusConflict, "FDL run is not dispatchable on this daemon")
		return
	}
	var snapshot struct {
		RoleBindings []fdlResolvedRoleBinding `json:"role_bindings"`
	}
	if json.Unmarshal(run.ProfileSnapshot, &snapshot) != nil {
		writeError(w, http.StatusInternalServerError, "invalid frozen FDL profile")
		return
	}
	var role *fdlResolvedRoleBinding
	for i := range snapshot.RoleBindings {
		if snapshot.RoleBindings[i].Role == request.Role {
			role = &snapshot.RoleBindings[i]
			break
		}
	}
	if role == nil || role.DaemonID != daemonID {
		writeError(w, http.StatusForbidden, "FDL role is not pinned to this daemon")
		return
	}
	agentID, ok := parseUUIDOrBadRequest(w, role.AgentID, "frozen_agent_id")
	if !ok {
		return
	}
	runtimeID, ok := parseUUIDOrBadRequest(w, role.RuntimeID, "frozen_runtime_id")
	if !ok {
		return
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
	if err != nil || agent.ArchivedAt.Valid || agent.RuntimeID != runtimeID {
		writeError(w, http.StatusConflict, "frozen FDL role runtime is no longer available")
		return
	}
	if existing, err := h.Queries.GetFDLAgentTaskByDispatchKey(r.Context(), db.GetFDLAgentTaskByDispatchKeyParams{IssueID: run.IssueID, RuntimeID: runtimeID, FdlDispatchKey: request.DispatchKey}); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"task_id": uuidToString(existing.ID), "status": existing.Status})
		return
	}
	// The frozen FDL snapshot is an audit projection, not the daemon's task
	// context wire shape. Translate it explicitly so a direct task validates the
	// same branch, head, and cleanliness facts the launch froze.
	var frozen struct {
		WorktreeID    string `json:"worktree_id"`
		DaemonID      string `json:"daemon_id"`
		LocalPath     string `json:"local_path"`
		CanonicalPath string `json:"canonical_path"`
		RepositoryURL string `json:"repository_url"`
		Branch        string `json:"branch"`
		HeadSHA       string `json:"head_sha"`
		MustBeClean   bool   `json:"must_be_clean"`
	}
	if json.Unmarshal(run.WorktreeSnapshot, &frozen) != nil || frozen.DaemonID != daemonID || frozen.LocalPath == "" || frozen.CanonicalPath == "" || frozen.RepositoryURL == "" || frozen.Branch == "" || frozen.HeadSHA == "" {
		writeError(w, http.StatusConflict, "frozen FDL worktree is invalid")
		return
	}
	worktreeContext, err := json.Marshal(service.WorktreeContext{
		WorktreeID: frozen.WorktreeID, DaemonID: frozen.DaemonID,
		LocalPath: frozen.LocalPath, CanonicalPath: frozen.CanonicalPath,
		RepositoryURL: frozen.RepositoryURL, ExpectedBranch: frozen.Branch,
		ExpectedHeadSHA: frozen.HeadSHA, MustBeClean: frozen.MustBeClean,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode FDL worktree context failed")
		return
	}
	hasExecutionWorkspace := len(bytes.TrimSpace(request.ExecutionWorkspace)) > 0 && !bytes.Equal(bytes.TrimSpace(request.ExecutionWorkspace), []byte("null"))
	if hasExecutionWorkspace {
		var execution fdlDaemonWorkspace
		decoder := json.NewDecoder(strings.NewReader(string(request.ExecutionWorkspace)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&execution) != nil || execution.SchemaVersion != 1 || execution.Kind != "fdl_isolated_workspace" || !execution.ReadOnly || execution.DaemonID != daemonID || execution.LocalPath == "" || execution.CanonicalPath == "" {
			writeError(w, http.StatusBadRequest, "invalid FDL isolated execution workspace")
			return
		}
		worktreeContext = request.ExecutionWorkspace
	}
	if (strings.HasPrefix(request.Role, "reviewer:") || strings.HasPrefix(request.Role, "explorer:")) && !hasExecutionWorkspace {
		writeError(w, http.StatusConflict, "FDL reviewer and explorer tasks require an isolated workspace")
		return
	}
	task, err := h.Queries.CreateFDLAgentTask(r.Context(), db.CreateFDLAgentTaskParams{
		AgentID: agentID, RuntimeID: runtimeID, IssueID: run.IssueID,
		Priority: request.Priority, HandoffNote: request.Instructions,
		FdlRole: request.Role, FdlDispatchKey: request.DispatchKey, WorktreeContext: worktreeContext,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create FDL agent task failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"task_id": uuidToString(task.ID), "status": task.Status})
}

// ActivateFDLAgentTaskForDaemon makes an already acknowledged direct task
// claimable. Before this call FDL tasks are deliberately invisible to normal
// task polling, so an Agent cannot start before Controller acknowledgement.
func (h *Handler) ActivateFDLAgentTaskForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_issue_run_id")
	if !ok {
		return
	}
	taskID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "taskId"), "task_id")
	if !ok {
		return
	}
	run, err := h.Queries.GetFDLIssueRunForDaemon(r.Context(), db.GetFDLIssueRunForDaemonParams{ID: runID, WorkspaceID: workspaceID, DaemonID: daemonID})
	if err != nil || run.Status != "running" {
		writeError(w, http.StatusConflict, "FDL run is not active on this daemon")
		return
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil || task.IssueID != run.IssueID || !service.IsFDLDirectTask(task) {
		writeError(w, http.StatusNotFound, "FDL task is not pending for this run")
		return
	}
	// The executor persists activation after this request. A crash in between
	// must not turn an already claimable direct task into a failed recovery.
	// Only the direct-task lifecycle states are accepted as idempotent success.
	if task.Status != "fdl_pending_ack" {
		switch task.Status {
		case "queued", "dispatched", "waiting_local_directory", "running", "completed", "failed", "cancelled":
			writeJSON(w, http.StatusOK, map[string]any{"task_id": uuidToString(task.ID), "status": task.Status})
			return
		default:
			writeError(w, http.StatusConflict, "FDL task cannot be activated")
			return
		}
	}
	activated, err := h.Queries.ActivateFDLAgentTask(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusConflict, "FDL task cannot be activated")
		return
	}
	h.TaskService.NotifyTaskEnqueued(r.Context(), activated)
	writeJSON(w, http.StatusOK, map[string]any{"task_id": uuidToString(activated.ID), "status": activated.Status})
}
