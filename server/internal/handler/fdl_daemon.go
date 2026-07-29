package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// daemonFDLIssueRunResponse carries frozen launch facts to the one daemon that
// owns the bound worktree. It is never returned from a workspace/user route.
type daemonFDLIssueRunResponse struct {
	ID               string          `json:"id"`
	IssueID          string          `json:"issue_id"`
	ProfileID        string          `json:"profile_id"`
	FDLRunID         *string         `json:"fdl_run_id,omitempty"`
	Status           string          `json:"status"`
	Phase            string          `json:"phase"`
	ProfileSnapshot  json.RawMessage `json:"profile_snapshot"`
	WorktreeSnapshot json.RawMessage `json:"worktree_snapshot"`
	RuntimeSnapshot  json.RawMessage `json:"runtime_snapshot"`
	IssueSnapshot    json.RawMessage `json:"issue_snapshot"`
}

const fdlRuntimeIdentityHeader = "X-Multica-FDL-Runtime-ID"

func daemonFDLIssueRunToResponse(run db.FdlIssueRun) daemonFDLIssueRunResponse {
	var fdlRunID *string
	if run.FdlRunID.Valid {
		id := run.FdlRunID.String
		fdlRunID = &id
	}
	return daemonFDLIssueRunResponse{
		ID:               uuidToString(run.ID),
		IssueID:          uuidToString(run.IssueID),
		ProfileID:        uuidToString(run.ProfileID),
		FDLRunID:         fdlRunID,
		Status:           run.Status,
		Phase:            run.Phase,
		ProfileSnapshot:  json.RawMessage(run.ProfileSnapshot),
		WorktreeSnapshot: json.RawMessage(run.WorktreeSnapshot),
		RuntimeSnapshot:  json.RawMessage(run.RuntimeSnapshot),
		IssueSnapshot:    json.RawMessage(run.IssueSnapshot),
	}
}

func (h *Handler) daemonFDLIdentity(w http.ResponseWriter, r *http.Request) (pgtype.UUID, string, bool) {
	workspaceID := middleware.DaemonWorkspaceIDFromContext(r.Context())
	daemonID := middleware.DaemonIDFromContext(r.Context())
	if workspaceID != "" && daemonID != "" {
		wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
		if !ok {
			return pgtype.UUID{}, "", false
		}
		return wsUUID, daemonID, true
	}

	// Existing daemons authenticate with an owner PAT rather than an mdt_
	// daemon token. A registered runtime lets that path prove both the owning
	// daemon and workspace without trusting a caller-supplied daemon ID.
	runtimeID := strings.TrimSpace(r.Header.Get(fdlRuntimeIdentityHeader))
	runtime, ok := h.requireDaemonRuntimeAccess(w, r, runtimeID)
	if !ok {
		return pgtype.UUID{}, "", false
	}
	if userID := requestUserID(r); userID == "" || !runtime.OwnerID.Valid || uuidToString(runtime.OwnerID) != userID || !runtime.DaemonID.Valid || strings.TrimSpace(runtime.DaemonID.String) == "" {
		writeError(w, http.StatusNotFound, "FDL daemon runtime is unavailable")
		return pgtype.UUID{}, "", false
	}
	return runtime.WorkspaceID, runtime.DaemonID.String, true
}

// ListPendingFDLIssueRunsForDaemon lets a daemon discover only runs whose
// frozen worktree is pinned to its own daemon ID.
func (h *Handler) ListPendingFDLIssueRunsForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runs, err := h.Queries.ListPendingFDLIssueRunsForDaemon(r.Context(), db.ListPendingFDLIssueRunsForDaemonParams{
		WorkspaceID: workspaceID,
		DaemonID:    daemonID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list pending FDL runs failed")
		return
	}
	response := make([]daemonFDLIssueRunResponse, len(runs))
	for i, run := range runs {
		response[i] = daemonFDLIssueRunToResponse(run)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": response})
}

// ListActiveFDLIssueRunsForDaemon gives the bound executor its own recovery
// set. It must never be used by a user-facing client to infer Controller state.
func (h *Handler) ListActiveFDLIssueRunsForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runs, err := h.Queries.ListActiveFDLIssueRunsForDaemon(r.Context(), db.ListActiveFDLIssueRunsForDaemonParams{
		WorkspaceID: workspaceID,
		DaemonID:    daemonID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list active FDL runs failed")
		return
	}
	response := make([]daemonFDLIssueRunResponse, len(runs))
	for i, run := range runs {
		response[i] = daemonFDLIssueRunToResponse(run)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": response})
}

type initializeFDLIssueRunRequest struct {
	FDLRunID string `json:"fdl_run_id"`
}

// InitializeFDLIssueRunForDaemon binds a locally initialized Controller run
// to its Multica projection in one transaction. The initial run root itself
// remains local and is never accepted or persisted by the server.
func (h *Handler) InitializeFDLIssueRunForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_issue_run_id")
	if !ok {
		return
	}
	var req initializeFDLIssueRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.FDLRunID) == "" || len(req.FDLRunID) > 256 {
		writeError(w, http.StatusBadRequest, "fdl_run_id is required")
		return
	}
	run, err := h.Queries.InitializeFDLIssueRunForDaemon(r.Context(), db.InitializeFDLIssueRunForDaemonParams{
		ID:          runID,
		WorkspaceID: workspaceID,
		FdlRunID:    pgtype.Text{String: strings.TrimSpace(req.FDLRunID), Valid: true},
		DaemonID:    daemonID,
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusConflict, "FDL run is unavailable, initialized, or belongs to another daemon")
			return
		}
		writeError(w, http.StatusInternalServerError, "initialize FDL run failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         uuidToString(run.ID),
		"issue_id":   uuidToString(run.IssueID),
		"fdl_run_id": run.FdlRunID.String,
		"status":     run.Status,
		"phase":      run.Phase,
	})
}

type fdlProjectionRequest struct {
	Status           string   `json:"status"`
	Phase            string   `json:"phase"`
	WaitingReason    string   `json:"waiting_reason,omitempty"`
	Summary          string   `json:"summary,omitempty"`
	ActionType       string   `json:"action_type,omitempty"`
	DecisionActionID string   `json:"decision_action_id,omitempty"`
	DecisionKind     string   `json:"decision_kind,omitempty"`
	AllowedDecisions []string `json:"allowed_decisions,omitempty"`
}

var allowedFDLProjectionStatuses = map[string]struct{}{
	"running": {}, "awaiting_human_decision": {}, "recovering": {}, "handoff_ready": {},
	"completed": {}, "failed": {}, "cancelled": {},
}

var allowedFDLProjectionPhases = map[string]struct{}{
	"setup": {}, "intake": {}, "pre_design": {}, "design": {}, "implementation": {},
	"gate": {}, "review": {}, "handoff": {}, "complete": {}, "cancelled": {},
}

func validateFDLProjectionRequest(req fdlProjectionRequest) (map[string]any, bool) {
	if _, ok := allowedFDLProjectionStatuses[req.Status]; !ok {
		return nil, false
	}
	if _, ok := allowedFDLProjectionPhases[req.Phase]; !ok {
		return nil, false
	}
	projection := make(map[string]any, 6)
	for key, value := range map[string]string{
		"waiting_reason": req.WaitingReason,
		"summary":        req.Summary,
		"action_type":    req.ActionType,
	} {
		value = strings.TrimSpace(value)
		if len(value) > 500 {
			return nil, false
		}
		if value != "" {
			projection[key] = value
		}
	}
	if req.DecisionActionID != "" || req.DecisionKind != "" || len(req.AllowedDecisions) > 0 {
		if (req.Status != "awaiting_human_decision" && req.Status != "recovering") || len(req.DecisionActionID) > 256 || len(req.DecisionKind) > 80 || req.DecisionActionID == "" || req.DecisionKind == "" || !validFDLDecisionChoices(req.AllowedDecisions) {
			return nil, false
		}
		projection["decision_action_id"] = req.DecisionActionID
		projection["decision_kind"] = req.DecisionKind
		projection["allowed_decisions"] = req.AllowedDecisions
	}
	return projection, true
}

func validFDLDecisionChoices(choices []string) bool {
	if len(choices) == 0 || len(choices) > 5 {
		return false
	}
	allowed := map[string]struct{}{"accept": {}, "reject": {}, "approve": {}, "retry": {}, "cancel": {}}
	seen := make(map[string]struct{}, len(choices))
	for _, choice := range choices {
		if _, ok := allowed[choice]; !ok {
			return false
		}
		if _, exists := seen[choice]; exists {
			return false
		}
		seen[choice] = struct{}{}
	}
	return true
}

// UpdateFDLIssueRunProjectionForDaemon writes only a small, display-safe
// projection. Controller state, evidence, tokens, and recovery authority stay
// in the external FDL run root.
func (h *Handler) UpdateFDLIssueRunProjectionForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_issue_run_id")
	if !ok {
		return
	}
	var req fdlProjectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	projection, valid := validateFDLProjectionRequest(req)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid FDL projection")
		return
	}
	projectionJSON, err := json.Marshal(projection)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "serialize FDL projection failed")
		return
	}
	run, err := h.Queries.UpdateFDLIssueRunProjectionForDaemon(r.Context(), db.UpdateFDLIssueRunProjectionForDaemonParams{
		ID:              runID,
		WorkspaceID:     workspaceID,
		Status:          req.Status,
		Phase:           req.Phase,
		StateProjection: projectionJSON,
		DaemonID:        daemonID,
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusConflict, "FDL run is unavailable, terminal, or belongs to another daemon")
			return
		}
		writeError(w, http.StatusInternalServerError, "update FDL projection failed")
		return
	}
	writeJSON(w, http.StatusOK, fdlIssueRunToResponse(run))
}

type daemonFDLHumanDecisionResponse struct {
	ID          string  `json:"id"`
	ActionID    string  `json:"action_id"`
	Decision    string  `json:"decision"`
	Status      string  `json:"status"`
	OperationID *string `json:"operation_id,omitempty"`
}

func daemonFDLHumanDecisionToResponse(decision db.FdlHumanDecision) daemonFDLHumanDecisionResponse {
	return daemonFDLHumanDecisionResponse{
		ID: uuidToString(decision.ID), ActionID: decision.ActionID, Decision: decision.Decision,
		Status: decision.Status, OperationID: textToPtr(decision.OperationID),
	}
}

// GetFDLHumanDecisionForDaemon returns only the current action-bound decision
// receipt to the daemon that owns this run's frozen worktree.
func (h *Handler) GetFDLHumanDecisionForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_issue_run_id")
	if !ok {
		return
	}
	actionID := strings.TrimSpace(chi.URLParam(r, "actionId"))
	if actionID == "" || len(actionID) > 256 {
		writeError(w, http.StatusBadRequest, "valid FDL action_id is required")
		return
	}
	decision, err := h.Queries.GetFDLHumanDecisionForDaemon(r.Context(), db.GetFDLHumanDecisionForDaemonParams{
		WorkspaceID: workspaceID, FdlIssueRunID: runID, ActionID: actionID, DaemonID: daemonID,
	})
	if err == pgx.ErrNoRows {
		writeError(w, http.StatusNotFound, "FDL decision is not pending")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load FDL decision failed")
		return
	}
	writeJSON(w, http.StatusOK, daemonFDLHumanDecisionToResponse(decision))
}

type claimFDLHumanDecisionRequest struct {
	OperationID string `json:"operation_id"`
}

func (h *Handler) ClaimFDLHumanDecisionForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_issue_run_id")
	if !ok {
		return
	}
	decisionID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "decisionId"), "fdl_human_decision_id")
	if !ok {
		return
	}
	var request claimFDLHumanDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(strings.TrimSpace(request.OperationID)) == 0 || len(request.OperationID) > 256 {
		writeError(w, http.StatusBadRequest, "valid FDL operation_id is required")
		return
	}
	decision, err := h.Queries.ClaimFDLHumanDecisionForDaemon(r.Context(), db.ClaimFDLHumanDecisionForDaemonParams{
		ID: decisionID, WorkspaceID: workspaceID, FdlIssueRunID: runID,
		OperationID: pgtype.Text{String: strings.TrimSpace(request.OperationID), Valid: true}, DaemonID: daemonID,
	})
	if err == pgx.ErrNoRows {
		existing, lookupErr := h.Queries.GetFDLHumanDecisionByIDForDaemon(r.Context(), db.GetFDLHumanDecisionByIDForDaemonParams{
			ID: decisionID, WorkspaceID: workspaceID, FdlIssueRunID: runID, DaemonID: daemonID,
		})
		if lookupErr == nil && existing.Status == "processing" && existing.OperationID.Valid && existing.OperationID.String == strings.TrimSpace(request.OperationID) {
			writeJSON(w, http.StatusOK, daemonFDLHumanDecisionToResponse(existing))
			return
		}
		writeError(w, http.StatusConflict, "FDL decision cannot be claimed")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "claim FDL decision failed")
		return
	}
	writeJSON(w, http.StatusOK, daemonFDLHumanDecisionToResponse(decision))
}

type completeFDLHumanDecisionRequest struct {
	OperationID string `json:"operation_id"`
}

func (h *Handler) CompleteFDLHumanDecisionForDaemon(w http.ResponseWriter, r *http.Request) {
	workspaceID, daemonID, ok := h.daemonFDLIdentity(w, r)
	if !ok {
		return
	}
	runID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_issue_run_id")
	if !ok {
		return
	}
	decisionID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "decisionId"), "fdl_human_decision_id")
	if !ok {
		return
	}
	var request completeFDLHumanDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(strings.TrimSpace(request.OperationID)) == 0 || len(request.OperationID) > 256 {
		writeError(w, http.StatusBadRequest, "valid FDL operation_id is required")
		return
	}
	decision, err := h.Queries.CompleteFDLHumanDecisionForDaemon(r.Context(), db.CompleteFDLHumanDecisionForDaemonParams{
		ID: decisionID, WorkspaceID: workspaceID, FdlIssueRunID: runID,
		OperationID: pgtype.Text{String: strings.TrimSpace(request.OperationID), Valid: true}, DaemonID: daemonID,
	})
	if err == pgx.ErrNoRows {
		existing, lookupErr := h.Queries.GetFDLHumanDecisionByIDForDaemon(r.Context(), db.GetFDLHumanDecisionByIDForDaemonParams{
			ID: decisionID, WorkspaceID: workspaceID, FdlIssueRunID: runID, DaemonID: daemonID,
		})
		if lookupErr == nil && existing.Status == "submitted" && existing.OperationID.Valid && existing.OperationID.String == strings.TrimSpace(request.OperationID) {
			writeJSON(w, http.StatusOK, daemonFDLHumanDecisionToResponse(existing))
			return
		}
		writeError(w, http.StatusConflict, "FDL decision cannot be completed")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "complete FDL decision failed")
		return
	}
	writeJSON(w, http.StatusOK, daemonFDLHumanDecisionToResponse(decision))
}

type fdlReasonRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) updateFDLProjectionReason(w http.ResponseWriter, r *http.Request, status, phase string) {
	var req fdlReasonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(strings.TrimSpace(req.Reason)) > 500 {
		writeError(w, http.StatusBadRequest, "a valid FDL reason is required")
		return
	}
	body, _ := json.Marshal(fdlProjectionRequest{Status: status, Phase: phase, WaitingReason: req.Reason})
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	h.UpdateFDLIssueRunProjectionForDaemon(w, r)
}

// CancelFDLIssueRunForDaemon records that the local Controller has stopped.
func (h *Handler) CancelFDLIssueRunForDaemon(w http.ResponseWriter, r *http.Request) {
	h.updateFDLProjectionReason(w, r, "cancelled", "cancelled")
}

// ReportFDLIssueRunRecoveryForDaemon records a recovery wait; only the local
// Controller decides whether and how a work item receives a fresh token.
func (h *Handler) ReportFDLIssueRunRecoveryForDaemon(w http.ResponseWriter, r *http.Request) {
	h.updateFDLProjectionReason(w, r, "recovering", "setup")
}
