package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type codeWorktreeResponse struct {
	ID            string `json:"id"`
	WorkspaceID   string `json:"workspace_id"`
	DaemonID      string `json:"daemon_id"`
	LocalPath     string `json:"local_path"`
	CanonicalPath string `json:"canonical_path"`
	RepositoryURL string `json:"repository_url"`
	Branch        string `json:"branch"`
	HeadSHA       string `json:"head_sha"`
	IsDirty       bool   `json:"is_dirty"`
	InspectedAt   string `json:"inspected_at"`
	UpdatedAt     string `json:"updated_at"`
}

func codeWorktreeToResponse(v db.CodeWorktree) codeWorktreeResponse {
	return codeWorktreeResponse{
		ID: uuidToString(v.ID), WorkspaceID: uuidToString(v.WorkspaceID), DaemonID: v.DaemonID,
		LocalPath: v.LocalPath, CanonicalPath: v.CanonicalPath, RepositoryURL: v.RepositoryUrl,
		Branch: v.Branch, HeadSHA: v.HeadSha, IsDirty: v.IsDirty,
		InspectedAt: timestampToString(v.InspectedAt), UpdatedAt: timestampToString(v.UpdatedAt),
	}
}

type inspectionRequest struct {
	DaemonID   string `json:"daemon_id"`
	LocalPath  string `json:"local_path"`
	WorktreeID string `json:"worktree_id,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}
type inspectionResultRequest struct {
	CanonicalPath string `json:"canonical_path"`
	RepositoryURL string `json:"repository_url"`
	Branch        string `json:"branch"`
	HeadSHA       string `json:"head_sha"`
	IsDirty       bool   `json:"is_dirty"`
	Error         string `json:"error,omitempty"`
}

type codeWorktreeInspectionResponse struct {
	ID            string  `json:"id"`
	DaemonID      string  `json:"daemon_id"`
	LocalPath     string  `json:"local_path"`
	WorktreeID    *string `json:"worktree_id"`
	Status        string  `json:"status"`
	CanonicalPath *string `json:"canonical_path"`
	RepositoryURL *string `json:"repository_url"`
	Branch        *string `json:"branch"`
	HeadSHA       *string `json:"head_sha"`
	IsDirty       *bool   `json:"is_dirty"`
	Error         *string `json:"error"`
	ExpiresAt     string  `json:"expires_at"`
	CompletedAt   *string `json:"completed_at"`
}

func codeWorktreeInspectionToResponse(v db.CodeWorktreeInspection) codeWorktreeInspectionResponse {
	return codeWorktreeInspectionResponse{
		ID: uuidToString(v.ID), DaemonID: v.DaemonID, LocalPath: v.LocalPath,
		WorktreeID: uuidToPtr(v.WorktreeID), Status: v.Status,
		CanonicalPath: textToPtr(v.CanonicalPath), RepositoryURL: textToPtr(v.RepositoryUrl),
		Branch: textToPtr(v.Branch), HeadSHA: textToPtr(v.HeadSha),
		IsDirty: nullableBoolToPtr(v.IsDirty), Error: textToPtr(v.Error),
		ExpiresAt: timestampToString(v.ExpiresAt), CompletedAt: timestampToPtr(v.CompletedAt),
	}
}

func nullableBoolToPtr(v pgtype.Bool) *bool {
	if !v.Valid {
		return nil
	}
	value := v.Bool
	return &value
}

func isGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') && !(char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func normalizeRepositoryURL(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if u, err := url.Parse(v); err == nil && u.Host != "" {
		return repositoryIdentity(strings.ToLower(u.Hostname()), u.Path)
	}
	if colon := strings.Index(v, ":"); colon > 0 && !strings.Contains(v[:colon], "/") {
		host := v[:colon]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return repositoryIdentity(strings.ToLower(host), v[colon+1:])
	}
	return strings.TrimSuffix(strings.TrimSuffix(v, "/"), ".git")
}

func repositoryIdentity(host, repoPath string) string {
	path := strings.Trim(strings.TrimSpace(repoPath), "/")
	path = strings.TrimSuffix(path, ".git")
	return host + "/" + path
}

func (h *Handler) CreateCodeWorktreeInspection(w http.ResponseWriter, r *http.Request) {
	wsID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	userUUID, _ := h.parseUserUUIDOrZero(userID)
	var req inspectionRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	req.DaemonID, req.LocalPath = strings.TrimSpace(req.DaemonID), strings.TrimSpace(req.LocalPath)
	if req.DaemonID == "" || !isAbsoluteLocalPath(req.LocalPath) {
		writeError(w, 400, "daemon_id and an absolute local_path are required")
		return
	}
	var worktreeID pgtype.UUID
	var expected pgtype.Timestamptz
	if req.WorktreeID != "" {
		worktreeID, ok = parseUUIDOrBadRequest(w, req.WorktreeID, "worktree_id")
		if !ok {
			return
		}
		wt, err := h.Queries.GetCodeWorktreeInWorkspace(r.Context(), db.GetCodeWorktreeInWorkspaceParams{ID: worktreeID, WorkspaceID: wsID})
		if err != nil || wt.DaemonID != req.DaemonID {
			writeError(w, 404, "code worktree not found")
			return
		}
		expected = wt.UpdatedAt
	}
	row, err := h.Queries.CreateCodeWorktreeInspection(r.Context(), db.CreateCodeWorktreeInspectionParams{WorkspaceID: wsID, DaemonID: req.DaemonID, LocalPath: req.LocalPath, WorktreeID: worktreeID, ExpectedUpdatedAt: expected, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(5 * time.Minute), Valid: true}, CreatedBy: userUUID})
	if err != nil {
		writeError(w, 500, "create code worktree inspection failed")
		return
	}
	writeJSON(w, http.StatusCreated, codeWorktreeInspectionToResponse(row))
}

func (h *Handler) CompleteCodeWorktreeInspection(w http.ResponseWriter, r *http.Request) {
	wsID := middleware.DaemonWorkspaceIDFromContext(r.Context())
	daemonID := middleware.DaemonIDFromContext(r.Context())
	if wsID == "" || daemonID == "" {
		writeError(w, 401, "daemon authentication required")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace_id")
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "inspection_id")
	if !ok {
		return
	}
	var req inspectionResultRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	if req.Error == "" && (strings.TrimSpace(req.CanonicalPath) == "" || strings.TrimSpace(req.RepositoryURL) == "" || strings.TrimSpace(req.Branch) == "" || !isGitSHA(strings.TrimSpace(req.HeadSHA))) {
		writeError(w, 400, "complete Git inspection facts are required")
		return
	}
	repositoryURL := normalizeRepositoryURL(req.RepositoryURL)
	row, err := h.Queries.CompleteCodeWorktreeInspection(r.Context(), db.CompleteCodeWorktreeInspectionParams{ID: id, WorkspaceID: wsUUID, CanonicalPath: pgtype.Text{String: strings.TrimSpace(req.CanonicalPath), Valid: req.Error == ""}, RepositoryUrl: pgtype.Text{String: repositoryURL, Valid: req.Error == ""}, Branch: pgtype.Text{String: strings.TrimSpace(req.Branch), Valid: req.Error == ""}, HeadSha: pgtype.Text{String: strings.TrimSpace(req.HeadSHA), Valid: req.Error == ""}, IsDirty: pgtype.Bool{Bool: req.IsDirty, Valid: req.Error == ""}, Column8: strings.TrimSpace(req.Error), DaemonID: daemonID})
	if err != nil {
		writeError(w, 409, "inspection is not pending or has expired")
		return
	}
	writeJSON(w, 200, map[string]any{"id": uuidToString(row.ID), "status": row.Status})
}

func (h *Handler) GetCodeWorktreeInspection(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "inspection_id")
	if !ok {
		return
	}
	inspection, err := h.Queries.GetCodeWorktreeInspectionInWorkspace(r.Context(), db.GetCodeWorktreeInspectionInWorkspaceParams{ID: id, WorkspaceID: ws})
	if err != nil {
		writeError(w, http.StatusNotFound, "code worktree inspection not found")
		return
	}
	writeJSON(w, http.StatusOK, codeWorktreeInspectionToResponse(inspection))
}

func (h *Handler) ListCodeWorktrees(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListCodeWorktrees(r.Context(), ws)
	if err != nil {
		writeError(w, 500, "list code worktrees failed")
		return
	}
	out := make([]codeWorktreeResponse, len(rows))
	for i, row := range rows {
		out[i] = codeWorktreeToResponse(row)
	}
	writeJSON(w, 200, map[string]any{"worktrees": out, "total": len(out)})
}

func (h *Handler) CreateCodeWorktree(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	user, ok := requireUserID(w, r)
	if !ok {
		return
	}
	userUUID, _ := h.parseUserUUIDOrZero(user)
	var body struct {
		InspectionID string `json:"inspection_id"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	id, ok := parseUUIDOrBadRequest(w, body.InspectionID, "inspection_id")
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "begin transaction failed")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	ins, err := q.ConsumeCodeWorktreeInspection(r.Context(), db.ConsumeCodeWorktreeInspectionParams{ID: id, WorkspaceID: ws})
	if err != nil || !ins.CanonicalPath.Valid || uuidToString(ins.CreatedBy) != uuidToString(userUUID) {
		writeError(w, 409, "a completed inspection created by this user is required")
		return
	}
	row, err := q.CreateCodeWorktree(r.Context(), db.CreateCodeWorktreeParams{WorkspaceID: ws, DaemonID: ins.DaemonID, LocalPath: ins.LocalPath, CanonicalPath: ins.CanonicalPath.String, RepositoryUrl: ins.RepositoryUrl.String, Branch: ins.Branch.String, HeadSha: ins.HeadSha.String, IsDirty: ins.IsDirty.Bool, CreatedBy: userUUID})
	if err != nil {
		writeError(w, 409, "code worktree already exists")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "create code worktree failed")
		return
	}
	writeJSON(w, 201, codeWorktreeToResponse(row))
}

func (h *Handler) GetCodeWorktree(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "worktree_id")
	if !ok {
		return
	}
	row, err := h.Queries.GetCodeWorktreeInWorkspace(r.Context(), db.GetCodeWorktreeInWorkspaceParams{ID: id, WorkspaceID: ws})
	if err != nil {
		writeError(w, 404, "code worktree not found")
		return
	}
	writeJSON(w, 200, codeWorktreeToResponse(row))
}

func (h *Handler) RefreshCodeWorktree(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	userUUID, _ := h.parseUserUUIDOrZero(userID)
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "worktree_id")
	if !ok {
		return
	}
	var body struct {
		InspectionID string `json:"inspection_id"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	inspectionID, ok := parseUUIDOrBadRequest(w, body.InspectionID, "inspection_id")
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "begin transaction failed")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	inspection, err := q.GetCodeWorktreeInspectionInWorkspace(r.Context(), db.GetCodeWorktreeInspectionInWorkspaceParams{ID: inspectionID, WorkspaceID: ws})
	if err != nil || inspection.Status != "completed" || uuidToString(inspection.WorktreeID) != uuidToString(id) || uuidToString(inspection.CreatedBy) != uuidToString(userUUID) || !inspection.CanonicalPath.Valid {
		writeError(w, 409, "a completed refresh inspection created by this user is required")
		return
	}
	updated, err := q.UpdateCodeWorktreeFromInspection(r.Context(), db.UpdateCodeWorktreeFromInspectionParams{ID: id, WorkspaceID: ws, LocalPath: inspection.LocalPath, CanonicalPath: inspection.CanonicalPath.String, RepositoryUrl: inspection.RepositoryUrl.String, Branch: inspection.Branch.String, HeadSha: inspection.HeadSha.String, IsDirty: inspection.IsDirty.Bool, UpdatedAt: inspection.ExpectedUpdatedAt})
	if err != nil {
		writeError(w, 409, "code worktree changed during inspection; inspect again")
		return
	}
	if _, err := q.ConsumeCodeWorktreeInspection(r.Context(), db.ConsumeCodeWorktreeInspectionParams{ID: inspectionID, WorkspaceID: ws}); err != nil {
		writeError(w, 409, "inspection was already consumed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "refresh code worktree failed")
		return
	}
	writeJSON(w, 200, codeWorktreeToResponse(updated))
}

func (h *Handler) DeleteCodeWorktree(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "worktree_id")
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "begin transaction failed")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	if _, err := q.LockCodeWorktreeInWorkspace(r.Context(), db.LockCodeWorktreeInWorkspaceParams{ID: id, WorkspaceID: ws}); err != nil {
		writeError(w, 404, "code worktree not found")
		return
	}
	projects, err := q.CountProjectsUsingCodeWorktree(r.Context(), id)
	if err != nil {
		writeError(w, 500, "check code worktree bindings failed")
		return
	}
	if projects > 0 {
		writeError(w, 409, "unbind this code worktree from projects before deleting it")
		return
	}
	active, err := q.CountActiveTasksUsingCodeWorktree(r.Context(), uuidToString(id))
	if err != nil {
		writeError(w, 500, "check active code worktree tasks failed")
		return
	}
	if active > 0 {
		writeError(w, 409, "wait for active tasks using this code worktree before deleting it")
		return
	}
	if err := q.DeleteCodeWorktreeInWorkspace(r.Context(), db.DeleteCodeWorktreeInWorkspaceParams{ID: id, WorkspaceID: ws}); err != nil {
		writeError(w, 500, "delete code worktree failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "delete code worktree failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) SetProjectCodeWorktree(w http.ResponseWriter, r *http.Request) {
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var body struct {
		WorktreeID *string `json:"worktree_id"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var id pgtype.UUID
	if body.WorktreeID != nil && *body.WorktreeID != "" {
		var valid bool
		id, valid = parseUUIDOrBadRequest(w, *body.WorktreeID, "worktree_id")
		if !valid {
			return
		}
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "begin transaction failed")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	project, err = q.LockProjectForCodeWorktree(r.Context(), db.LockProjectForCodeWorktreeParams{ID: project.ID, WorkspaceID: project.WorkspaceID})
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}
	if id.Valid {
		worktree, err := q.LockCodeWorktreeInWorkspace(r.Context(), db.LockCodeWorktreeInWorkspaceParams{ID: id, WorkspaceID: project.WorkspaceID})
		if err != nil {
			writeError(w, 404, "code worktree not found")
			return
		}
		resources, err := q.ListProjectResources(r.Context(), project.ID)
		if err != nil {
			writeError(w, 500, "check project resources failed")
			return
		}
		for _, resource := range resources {
			if resource.ResourceType != "local_directory" {
				continue
			}
			var local localDirectoryRef
			if json.Unmarshal(resource.ResourceRef, &local) == nil && local.DaemonID == worktree.DaemonID {
				writeError(w, 409, "remove the legacy local_directory on this daemon before binding a code worktree")
				return
			}
		}
	}
	updated, err := q.SetProjectDefaultCodeWorktree(r.Context(), db.SetProjectDefaultCodeWorktreeParams{ID: project.ID, WorkspaceID: project.WorkspaceID, DefaultCodeWorktreeID: id})
	if err != nil {
		writeError(w, 500, "set code worktree failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "set code worktree failed")
		return
	}
	h.publish(protocol.EventProjectUpdated, uuidToString(project.WorkspaceID), "member", userID, map[string]any{"project": projectToResponse(updated)})
	writeJSON(w, 200, projectToResponse(updated))
}
