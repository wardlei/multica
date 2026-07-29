package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const fdlProfileSchemaVersion = 1

var requiredFDLRoles = []string{
	"intake",
	"planner",
	"implementer",
	"reviewer:correctness",
	"reviewer:regression",
	"reviewer:specialist",
	"explorer:impact_analysis",
}

type fdlRoleBinding struct {
	Role    string `json:"role"`
	AgentID string `json:"agent_id"`
}

type fdlResolvedRoleBinding struct {
	Role          string `json:"role"`
	AgentID       string `json:"agent_id"`
	AgentName     string `json:"agent_name"`
	RuntimeID     string `json:"runtime_id"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinking_level"`
	ServiceTier   string `json:"service_tier"`
	DaemonID      string `json:"daemon_id,omitempty"`
	// agent is retained only while the launch contract is validated. It is
	// intentionally unexported so it cannot enter the frozen/public snapshot.
	agent db.Agent
}

type FDLDeliveryProfileResponse struct {
	ID               string          `json:"id"`
	WorkspaceID      string          `json:"workspace_id"`
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	SquadID          string          `json:"squad_id"`
	ControllerConfig json.RawMessage `json:"controller_config"`
	RoleBindings     json.RawMessage `json:"role_bindings"`
	CreatedBy        string          `json:"created_by"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
}

// FDLIssueRunResponse is a read-only projection for the Issue UI. It never
// exposes controller config, absolute worktree paths, result directories, or
// submission tokens; those remain local executor state.
type FDLIssueRunResponse struct {
	ID              string          `json:"id"`
	IssueID         string          `json:"issue_id"`
	ProfileID       string          `json:"profile_id"`
	FDLRunID        *string         `json:"fdl_run_id"`
	Status          string          `json:"status"`
	Phase           string          `json:"phase"`
	StateProjection json.RawMessage `json:"state_projection"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
	StartedAt       *string         `json:"started_at"`
	FinishedAt      *string         `json:"finished_at"`
}

type createFDLDeliveryProfileRequest struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	SquadID          string          `json:"squad_id"`
	ControllerConfig json.RawMessage `json:"controller_config"`
	RoleBindings     json.RawMessage `json:"role_bindings"`
}

type updateFDLDeliveryProfileRequest struct {
	Name             *string         `json:"name"`
	Description      *string         `json:"description"`
	SquadID          *string         `json:"squad_id"`
	ControllerConfig json.RawMessage `json:"controller_config"`
	RoleBindings     json.RawMessage `json:"role_bindings"`
}

func fdlDeliveryProfileToResponse(profile db.FdlDeliveryProfile) FDLDeliveryProfileResponse {
	return FDLDeliveryProfileResponse{
		ID:               uuidToString(profile.ID),
		WorkspaceID:      uuidToString(profile.WorkspaceID),
		Name:             profile.Name,
		Description:      profile.Description,
		SquadID:          uuidToString(profile.SquadID),
		ControllerConfig: json.RawMessage(profile.ControllerConfig),
		RoleBindings:     json.RawMessage(profile.RoleBindings),
		CreatedBy:        uuidToString(profile.CreatedBy),
		CreatedAt:        timestampToString(profile.CreatedAt),
		UpdatedAt:        timestampToString(profile.UpdatedAt),
	}
}

func fdlIssueRunToResponse(run db.FdlIssueRun) FDLIssueRunResponse {
	return FDLIssueRunResponse{
		ID:              uuidToString(run.ID),
		IssueID:         uuidToString(run.IssueID),
		ProfileID:       uuidToString(run.ProfileID),
		FDLRunID:        textToPtr(run.FdlRunID),
		Status:          run.Status,
		Phase:           run.Phase,
		StateProjection: json.RawMessage(run.StateProjection),
		CreatedAt:       timestampToString(run.CreatedAt),
		UpdatedAt:       timestampToString(run.UpdatedAt),
		StartedAt:       timestampToPtr(run.StartedAt),
		FinishedAt:      timestampToPtr(run.FinishedAt),
	}
}

// GetFDLIssueRun returns the server's non-authoritative status projection for
// one FDL issue. The FDL run root remains authoritative for all transitions.
func (h *Handler) GetFDLIssueRun(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if issue.OrchestrationMode != "fdl" {
		writeError(w, http.StatusNotFound, "issue does not use FDL delivery")
		return
	}
	run, err := h.Queries.GetFDLIssueRunByIssue(r.Context(), db.GetFDLIssueRunByIssueParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "FDL delivery run not found")
		return
	}
	writeJSON(w, http.StatusOK, fdlIssueRunToResponse(run))
}

type submitFDLHumanDecisionRequest struct {
	ActionID string `json:"action_id"`
	Decision string `json:"decision"`
}

type fdlDecisionProjection struct {
	DecisionActionID string   `json:"decision_action_id"`
	AllowedDecisions []string `json:"allowed_decisions"`
}

// SubmitFDLHumanDecision records a structured user choice for the local
// executor. It deliberately does not drive the Controller: only the daemon
// that owns the frozen worktree can bind this receipt to a mailbox action.
func (h *Handler) SubmitFDLHumanDecision(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if issue.OrchestrationMode != "fdl" {
		writeError(w, http.StatusNotFound, "issue does not use FDL delivery")
		return
	}
	var request submitFDLHumanDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid FDL decision request")
		return
	}
	request.ActionID = strings.TrimSpace(request.ActionID)
	request.Decision = strings.TrimSpace(request.Decision)
	if request.ActionID == "" || len(request.ActionID) > 256 || request.Decision == "" || len(request.Decision) > 32 {
		writeError(w, http.StatusBadRequest, "a valid FDL decision action is required")
		return
	}
	run, err := h.Queries.GetFDLIssueRunByIssue(r.Context(), db.GetFDLIssueRunByIssueParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusNotFound, "FDL delivery run not found")
		return
	}
	var projection fdlDecisionProjection
	if json.Unmarshal(run.StateProjection, &projection) != nil || run.Status != "awaiting_human_decision" || projection.DecisionActionID != request.ActionID || !fdlDecisionAllowed(projection.AllowedDecisions, request.Decision) {
		writeError(w, http.StatusConflict, "FDL decision action is no longer current")
		return
	}
	member, ok := h.requireWorkspaceMember(w, r, uuidToString(issue.WorkspaceID), "workspace not found")
	if !ok {
		return
	}
	decision, err := h.Queries.CreateFDLHumanDecision(r.Context(), db.CreateFDLHumanDecisionParams{
		WorkspaceID: issue.WorkspaceID, FdlIssueRunID: run.ID, ActionID: request.ActionID,
		Decision: request.Decision, SubmittedBy: member.UserID,
	})
	if err == pgx.ErrNoRows {
		existing, lookupErr := h.Queries.GetFDLHumanDecisionByAction(r.Context(), db.GetFDLHumanDecisionByActionParams{
			WorkspaceID: issue.WorkspaceID, FdlIssueRunID: run.ID, ActionID: request.ActionID,
		})
		if lookupErr != nil || existing.Decision != request.Decision {
			writeError(w, http.StatusConflict, "a different FDL decision is already recorded")
			return
		}
		decision = existing
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "record FDL decision failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": uuidToString(decision.ID), "status": decision.Status})
}

func fdlDecisionAllowed(allowed []string, decision string) bool {
	for _, candidate := range allowed {
		if candidate == decision {
			return true
		}
	}
	return false
}

// ListFDLDeliveryProfiles returns current workflow templates. The template is
// mutable, but a run always snapshots its exact content at issue creation.
func (h *Handler) ListFDLDeliveryProfiles(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}
	profiles, err := h.Queries.ListFDLDeliveryProfiles(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list FDL Delivery Profiles")
		return
	}
	response := make([]FDLDeliveryProfileResponse, 0, len(profiles))
	for _, profile := range profiles {
		response = append(response, fdlDeliveryProfileToResponse(profile))
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) CreateFDLDeliveryProfile(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	var request createFDLDeliveryProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	squadID, ok := parseUUIDOrBadRequest(w, request.SquadID, "squad_id")
	if !ok {
		return
	}
	squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})
	if err != nil || squad.ArchivedAt.Valid {
		writeError(w, http.StatusBadRequest, "squad is not available in this workspace")
		return
	}
	if !canManageSquad(member, squad) {
		writeError(w, http.StatusForbidden, "insufficient permissions to configure this squad")
		return
	}
	if _, err := validateFDLControllerConfig(request.ControllerConfig); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := h.resolveFDLRoleBindings(r.Context(), wsUUID, squad, request.RoleBindings, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	profile, err := h.Queries.CreateFDLDeliveryProfile(r.Context(), db.CreateFDLDeliveryProfileParams{
		WorkspaceID:      wsUUID,
		Name:             request.Name,
		Description:      strings.TrimSpace(request.Description),
		SquadID:          squad.ID,
		ControllerConfig: request.ControllerConfig,
		RoleBindings:     request.RoleBindings,
		CreatedBy:        member.UserID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create FDL Delivery Profile")
		return
	}
	writeJSON(w, http.StatusCreated, fdlDeliveryProfileToResponse(profile))
}

// GetFDLDeliveryProfile returns the current mutable template. Active FDL runs
// never read this endpoint after launch; they use their immutable snapshot.
func (h *Handler) GetFDLDeliveryProfile(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	profileID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_delivery_profile_id")
	if !ok {
		return
	}
	profile, err := h.Queries.GetFDLDeliveryProfileInWorkspace(r.Context(), db.GetFDLDeliveryProfileInWorkspaceParams{
		ID: profileID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "FDL Delivery Profile not found")
		return
	}
	writeJSON(w, http.StatusOK, fdlDeliveryProfileToResponse(profile))
}

// UpdateFDLDeliveryProfile changes a future-run template only. It validates the
// complete merged template against the selected squad before storing it.
func (h *Handler) UpdateFDLDeliveryProfile(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	profileID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_delivery_profile_id")
	if !ok {
		return
	}
	profile, err := h.Queries.GetFDLDeliveryProfileInWorkspace(r.Context(), db.GetFDLDeliveryProfileInWorkspaceParams{
		ID: profileID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "FDL Delivery Profile not found")
		return
	}
	currentSquad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{ID: profile.SquadID, WorkspaceID: wsUUID})
	if err != nil || currentSquad.ArchivedAt.Valid || !canManageSquad(member, currentSquad) {
		writeError(w, http.StatusForbidden, "insufficient permissions to configure this squad")
		return
	}
	var request updateFDLDeliveryProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	targetSquad := currentSquad
	if request.SquadID != nil {
		squadID, ok := parseUUIDOrBadRequest(w, *request.SquadID, "squad_id")
		if !ok {
			return
		}
		targetSquad, err = h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})
		if err != nil || targetSquad.ArchivedAt.Valid {
			writeError(w, http.StatusBadRequest, "squad is not available in this workspace")
			return
		}
		if !canManageSquad(member, targetSquad) {
			writeError(w, http.StatusForbidden, "insufficient permissions to configure this squad")
			return
		}
	}
	controllerConfig := profile.ControllerConfig
	if len(request.ControllerConfig) > 0 {
		controllerConfig = request.ControllerConfig
	}
	if _, err := validateFDLControllerConfig(controllerConfig); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	roleBindings := profile.RoleBindings
	if len(request.RoleBindings) > 0 {
		roleBindings = request.RoleBindings
	}
	if _, err := h.resolveFDLRoleBindings(r.Context(), wsUUID, targetSquad, roleBindings, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	params := db.UpdateFDLDeliveryProfileParams{ID: profile.ID, WorkspaceID: wsUUID}
	if request.Name != nil {
		params.Name = pgtype.Text{String: strings.TrimSpace(*request.Name), Valid: true}
		if params.Name.String == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
	}
	if request.Description != nil {
		params.Description = pgtype.Text{String: strings.TrimSpace(*request.Description), Valid: true}
	}
	if request.SquadID != nil {
		params.SquadID = targetSquad.ID
	}
	if len(request.ControllerConfig) > 0 {
		params.ControllerConfig = controllerConfig
	}
	if len(request.RoleBindings) > 0 {
		params.RoleBindings = roleBindings
	}
	updated, err := h.Queries.UpdateFDLDeliveryProfile(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update FDL Delivery Profile")
		return
	}
	writeJSON(w, http.StatusOK, fdlDeliveryProfileToResponse(updated))
}

// ArchiveFDLDeliveryProfile makes a template unavailable to new Issues. It
// does not cancel or alter any run that already froze this template.
func (h *Handler) ArchiveFDLDeliveryProfile(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	profileID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "fdl_delivery_profile_id")
	if !ok {
		return
	}
	profile, err := h.Queries.GetFDLDeliveryProfileInWorkspace(r.Context(), db.GetFDLDeliveryProfileInWorkspaceParams{
		ID: profileID, WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "FDL Delivery Profile not found")
		return
	}
	squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{ID: profile.SquadID, WorkspaceID: wsUUID})
	if err != nil || !canManageSquad(member, squad) {
		writeError(w, http.StatusForbidden, "insufficient permissions to configure this squad")
		return
	}
	if _, err := h.Queries.ArchiveFDLDeliveryProfile(r.Context(), db.ArchiveFDLDeliveryProfileParams{
		ID: profile.ID, WorkspaceID: wsUUID, ArchivedBy: member.UserID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to archive FDL Delivery Profile")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateFDLControllerConfig validates the server-safe template shape. The
// executor adds the local repo_root/run_root and FDL itself remains the final
// config validator before it initializes a run.
func validateFDLControllerConfig(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var config map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &config) != nil {
		return nil, errors.New("controller_config must be a JSON object")
	}
	var schemaVersion int
	if value, ok := config["schema_version"]; !ok || json.Unmarshal(value, &schemaVersion) != nil || schemaVersion != 4 {
		return nil, errors.New("controller_config.schema_version must be 4")
	}
	for _, key := range []string{"change_rules", "control_files", "workspace_policy", "capability_profiles", "gates", "workflow_policy", "contract_test_map", "known_failure_catalog"} {
		if _, ok := config[key]; !ok {
			return nil, fmt.Errorf("controller_config.%s is required", key)
		}
	}
	if _, ok := config["repo_root"]; ok {
		return nil, errors.New("controller_config.repo_root is derived from the issue worktree")
	}
	if _, ok := config["run_root"]; ok {
		return nil, errors.New("controller_config.run_root is local executor state and must not be stored in a profile")
	}
	return config, nil
}

func parseFDLRoleBindings(raw json.RawMessage) ([]fdlRoleBinding, error) {
	var bindings []fdlRoleBinding
	if len(raw) == 0 || json.Unmarshal(raw, &bindings) != nil {
		return nil, errors.New("role_bindings must be a JSON array")
	}
	if len(bindings) != len(requiredFDLRoles) {
		return nil, fmt.Errorf("role_bindings must contain exactly these roles: %s", strings.Join(requiredFDLRoles, ", "))
	}
	expected := make(map[string]struct{}, len(requiredFDLRoles))
	for _, role := range requiredFDLRoles {
		expected[role] = struct{}{}
	}
	seen := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if _, ok := expected[binding.Role]; !ok {
			return nil, fmt.Errorf("unsupported FDL role %q", binding.Role)
		}
		if _, duplicate := seen[binding.Role]; duplicate {
			return nil, fmt.Errorf("FDL role %q is configured more than once", binding.Role)
		}
		if _, err := util.ParseUUID(binding.AgentID); err != nil {
			return nil, fmt.Errorf("FDL role %q has an invalid agent_id", binding.Role)
		}
		seen[binding.Role] = struct{}{}
	}
	for _, role := range requiredFDLRoles {
		if _, ok := seen[role]; !ok {
			return nil, fmt.Errorf("FDL role %q is required", role)
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Role < bindings[j].Role })
	return bindings, nil
}

func (h *Handler) resolveFDLRoleBindings(ctx context.Context, workspaceID pgtype.UUID, squad db.Squad, raw json.RawMessage, requireWorktreeDaemon bool) ([]fdlResolvedRoleBinding, error) {
	bindings, err := parseFDLRoleBindings(raw)
	if err != nil {
		return nil, err
	}
	members, err := h.Queries.ListSquadMembers(ctx, squad.ID)
	if err != nil {
		return nil, fmt.Errorf("load squad members: %w", err)
	}
	memberAgents := map[string]struct{}{uuidToString(squad.LeaderID): {}}
	for _, member := range members {
		if member.MemberType == "agent" {
			memberAgents[uuidToString(member.MemberID)] = struct{}{}
		}
	}
	resolved := make([]fdlResolvedRoleBinding, 0, len(bindings))
	for _, binding := range bindings {
		if _, ok := memberAgents[binding.AgentID]; !ok {
			return nil, fmt.Errorf("FDL role %q must use an agent in the selected squad", binding.Role)
		}
		agentID, _ := util.ParseUUID(binding.AgentID)
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
		if err != nil || agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
			return nil, fmt.Errorf("FDL role %q has no active runtime", binding.Role)
		}
		if requireWorktreeDaemon {
			runtime, err := h.Queries.GetAgentRuntime(ctx, agent.RuntimeID)
			if err != nil || !runtime.DaemonID.Valid || runtime.DaemonID.String == "" {
				return nil, fmt.Errorf("FDL role %q runtime is unavailable", binding.Role)
			}
			resolved = append(resolved, fdlResolvedRoleBinding{
				Role: binding.Role, AgentID: uuidToString(agent.ID), AgentName: agent.Name,
				RuntimeID: uuidToString(agent.RuntimeID), Model: agent.Model.String,
				ThinkingLevel: agent.ThinkingLevel.String, ServiceTier: agent.ServiceTier.String,
				DaemonID: runtime.DaemonID.String, agent: agent,
			})
			continue
		}
		resolved = append(resolved, fdlResolvedRoleBinding{
			Role: binding.Role, AgentID: uuidToString(agent.ID), AgentName: agent.Name,
			RuntimeID: uuidToString(agent.RuntimeID), Model: agent.Model.String,
			ThinkingLevel: agent.ThinkingLevel.String, ServiceTier: agent.ServiceTier.String,
			agent: agent,
		})
	}
	return resolved, nil
}

func (h *Handler) prepareFDLIssueRun(ctx context.Context, workspaceID, profileID, projectID pgtype.UUID, member db.Member) (service.FDLIssueRunCreateParams, db.FdlDeliveryProfile, error) {
	profile, err := h.Queries.GetFDLDeliveryProfileInWorkspace(ctx, db.GetFDLDeliveryProfileInWorkspaceParams{ID: profileID, WorkspaceID: workspaceID})
	if err != nil {
		return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, errors.New("FDL Delivery Profile not found in this workspace")
	}
	if _, err := validateFDLControllerConfig(profile.ControllerConfig); err != nil {
		return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, fmt.Errorf("FDL Delivery Profile is invalid: %w", err)
	}
	squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: profile.SquadID, WorkspaceID: workspaceID})
	if err != nil || squad.ArchivedAt.Valid {
		return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, errors.New("FDL Delivery Profile squad is unavailable")
	}
	worktree, err := h.Queries.GetProjectCodeWorktree(ctx, db.GetProjectCodeWorktreeParams{ID: projectID, WorkspaceID: workspaceID})
	if err != nil || worktree.IsDirty {
		return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, errors.New("FDL delivery requires a project with a clean code worktree")
	}
	roles, err := h.resolveFDLRoleBindings(ctx, workspaceID, squad, profile.RoleBindings, true)
	if err != nil {
		return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, err
	}
	for _, role := range roles {
		// Do not use memberCanWireAgent here: workspace admins may configure a
		// squad, but canInvokeAgent deliberately gives them no bypass for a
		// private role Agent. An FDL launch is an actual invocation of every
		// frozen role, not merely a squad-management operation.
		memberID := uuidToString(member.UserID)
		if !h.canInvokeAgent(ctx, role.agent, "member", memberID, memberID, uuidToString(workspaceID)) {
			return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, fmt.Errorf("you do not have permission to invoke FDL role %q", role.Role)
		}
		if role.DaemonID != worktree.DaemonID {
			return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, fmt.Errorf("FDL role %q is not attached to the project worktree daemon", role.Role)
		}
	}
	profileSnapshot, err := json.Marshal(map[string]any{
		"schema_version":      fdlProfileSchemaVersion,
		"profile_id":          uuidToString(profile.ID),
		"profile_name":        profile.Name,
		"profile_description": profile.Description,
		"squad_id":            uuidToString(profile.SquadID),
		"controller_config":   json.RawMessage(profile.ControllerConfig),
		"role_bindings":       roles,
	})
	if err != nil {
		return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, fmt.Errorf("serialize FDL profile snapshot: %w", err)
	}
	worktreeSnapshot, err := json.Marshal(map[string]any{
		"worktree_id": worktree.ID.String(), "daemon_id": worktree.DaemonID,
		"local_path": worktree.LocalPath, "canonical_path": worktree.CanonicalPath,
		"repository_url": worktree.RepositoryUrl, "branch": worktree.Branch,
		"head_sha": worktree.HeadSha, "must_be_clean": true,
	})
	if err != nil {
		return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, fmt.Errorf("serialize FDL worktree snapshot: %w", err)
	}
	runtimeSnapshot, err := json.Marshal(map[string]any{
		"schema_version": 1, "daemon_id": worktree.DaemonID, "roles": roles,
	})
	if err != nil {
		return service.FDLIssueRunCreateParams{}, db.FdlDeliveryProfile{}, fmt.Errorf("serialize FDL runtime snapshot: %w", err)
	}
	return service.FDLIssueRunCreateParams{
		ProfileID: profile.ID, ProfileSnapshot: profileSnapshot,
		WorktreeSnapshot: worktreeSnapshot, RuntimeSnapshot: runtimeSnapshot,
	}, profile, nil
}
