package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

const fdlExecutorLeaseTTL = 2 * time.Minute

type fdlExecutorLease struct {
	SchemaVersion int       `json:"schema_version"`
	HolderID      string    `json:"holder_id"`
	ActionID      string    `json:"action_id,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// fdlExecutorMailbox is executor-private. Envelope data can contain
// submission tokens, so it lives outside both the repository and run root.
type fdlExecutorMailbox struct {
	SchemaVersion int             `json:"schema_version"`
	FDLRunID      string          `json:"fdl_run_id"`
	ActionID      string          `json:"action_id"`
	ActionKind    string          `json:"action_kind"`
	Envelope      json.RawMessage `json:"envelope"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// fdlWorkItemBinding is the local-only bridge from a Controller item to one
// Multica task. Its token must never appear in any task request, database row,
// comment, or Agent prompt.
type fdlWorkItemBinding struct {
	SchemaVersion       int       `json:"schema_version"`
	FDLRunID            string    `json:"fdl_run_id"`
	ActionID            string    `json:"action_id"`
	ExternalWorkID      string    `json:"external_work_id"`
	WorkItemID          string    `json:"work_item_id"`
	SubmissionToken     string    `json:"submission_token"`
	DispatchSHA256      string    `json:"dispatch_sha256"`
	DispatchKey         string    `json:"dispatch_key"`
	Role                string    `json:"role"`
	TaskID              string    `json:"task_id,omitempty"`
	State               string    `json:"state"`
	DispatchError       string    `json:"dispatch_error,omitempty"`
	TerminalOperationID string    `json:"terminal_operation_id,omitempty"`
	TerminalSubmitted   bool      `json:"terminal_submitted,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// fdlDispatchReceipt makes Controller acknowledgement and task activation one
// recoverable local transaction. It is deliberately outside the FDL run root:
// the Controller owns that root, while this receipt only records executor work.
type fdlDispatchReceipt struct {
	SchemaVersion          int               `json:"schema_version"`
	FDLRunID               string            `json:"fdl_run_id"`
	ActionID               string            `json:"action_id"`
	ExternalWorkID         string            `json:"external_work_id"`
	AckOperationID         string            `json:"ack_operation_id"`
	TaskIDs                map[string]string `json:"task_ids"`
	DispatchFailures       map[string]string `json:"dispatch_failures,omitempty"`
	ControllerAcknowledged bool              `json:"controller_acknowledged"`
	ReturnedEnvelope       json.RawMessage   `json:"returned_envelope,omitempty"`
	Activated              map[string]bool   `json:"activated"`
	UpdatedAt              time.Time         `json:"updated_at"`
}

type fdlCancellationReceipt struct {
	SchemaVersion int       `json:"schema_version"`
	FDLRunID      string    `json:"fdl_run_id"`
	OperationID   string    `json:"operation_id"`
	Terminated    bool      `json:"terminated"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// fdlDecisionReceipt preserves the server-side choice and its fixed Controller
// operation ID across a daemon crash. It stays in executor-private storage.
type fdlDecisionReceipt struct {
	SchemaVersion       int             `json:"schema_version"`
	DecisionID          string          `json:"decision_id"`
	FDLRunID            string          `json:"fdl_run_id"`
	ActionID            string          `json:"action_id"`
	Decision            string          `json:"decision"`
	OperationID         string          `json:"operation_id"`
	ControllerSubmitted bool            `json:"controller_submitted"`
	ReturnedEnvelope    json.RawMessage `json:"returned_envelope,omitempty"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// fdlHandoffReceipt retains the verified delivery handoff outside the FDL run
// root. Git, PR, and release workflows may consume it later, but never append
// their state to the Controller run.
type fdlHandoffReceipt struct {
	SchemaVersion int             `json:"schema_version"`
	FDLRunID      string          `json:"fdl_run_id"`
	ActionID      string          `json:"action_id"`
	Verified      bool            `json:"verified"`
	Handoff       json.RawMessage `json:"handoff"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// fdlRecoveryReceipt binds one explicit human retry/cancel choice to the
// failed Controller work item. The token stays local even while the choice is
// visible as a safe action ID in the UI.
type fdlRecoveryReceipt struct {
	SchemaVersion       int             `json:"schema_version"`
	DecisionID          string          `json:"decision_id"`
	FDLRunID            string          `json:"fdl_run_id"`
	ActionID            string          `json:"action_id"`
	ExternalWorkID      string          `json:"external_work_id"`
	WorkItemID          string          `json:"work_item_id"`
	SubmissionToken     string          `json:"submission_token"`
	Resolution          string          `json:"resolution"`
	OperationID         string          `json:"operation_id"`
	ControllerSubmitted bool            `json:"controller_submitted"`
	ReturnedEnvelope    json.RawMessage `json:"returned_envelope,omitempty"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// fdlExecutorLoop owns the local side of a frozen Controller run. It never
// derives work from comments or legacy Squad leader behavior.
func (d *Daemon) fdlExecutorLoop(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.FDLPollInterval)
	defer ticker.Stop()
	for {
		d.runFDLExecutorCycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Daemon) runFDLExecutorCycle(ctx context.Context) {
	for _, runtimeID := range d.fdlExecutorRuntimeIDs() {
		runs, err := d.client.ListPendingFDLIssueRuns(ctx, runtimeID)
		if err != nil {
			d.logger.Warn("FDL pending run poll failed", "runtime_id", runtimeID, "error", err)
			continue
		}
		for _, run := range runs {
			if err := d.initializeFDLRun(ctx, run); err != nil {
				d.logger.Warn("FDL run initialization failed", "fdl_issue_run_id", run.ID, "issue_id", run.IssueID, "error", err)
				if reportErr := d.client.ReportFDLIssueRunRecovery(ctx, run.FDLRuntimeID, run.ID, "executor initialization failed: "+boundedFDLError(err)); reportErr != nil {
					d.logger.Warn("FDL recovery projection failed", "fdl_issue_run_id", run.ID, "error", reportErr)
				}
			}
		}
		active, err := d.client.ListActiveFDLIssueRuns(ctx, runtimeID)
		if err != nil {
			d.logger.Warn("FDL active run poll failed", "runtime_id", runtimeID, "error", err)
			continue
		}
		for _, run := range active {
			if err := d.refreshFDLMailbox(ctx, run); err != nil {
				d.logger.Warn("FDL mailbox refresh failed", "fdl_issue_run_id", run.ID, "issue_id", run.IssueID, "error", err)
			}
		}
	}
}

// fdlExecutorRuntimeIDs chooses one registered runtime per workspace. The
// runtime authenticates FDL requests while the frozen worktree still selects
// which daemon may receive an individual run.
func (d *Daemon) fdlExecutorRuntimeIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspaceIDs := make([]string, 0, len(d.workspaces))
	for workspaceID := range d.workspaces {
		workspaceIDs = append(workspaceIDs, workspaceID)
	}
	sort.Strings(workspaceIDs)
	runtimeIDs := make([]string, 0, len(workspaceIDs))
	for _, workspaceID := range workspaceIDs {
		ids := append([]string(nil), d.workspaces[workspaceID].runtimeIDs...)
		sort.Strings(ids)
		if len(ids) > 0 {
			runtimeIDs = append(runtimeIDs, ids[0])
		}
	}
	return runtimeIDs
}

func (d *Daemon) initializeFDLRun(ctx context.Context, run PendingFDLIssueRun) error {
	if err := os.MkdirAll(d.cfg.FDLRunRoot, 0o700); err != nil {
		return fmt.Errorf("create FDL run root: %w", err)
	}
	var profile struct {
		ControllerConfig map[string]any `json:"controller_config"`
	}
	if err := json.Unmarshal(run.ProfileSnapshot, &profile); err != nil || profile.ControllerConfig == nil {
		return fmt.Errorf("decode frozen controller config")
	}
	var worktree struct {
		LocalPath string `json:"local_path"`
	}
	if err := json.Unmarshal(run.WorktreeSnapshot, &worktree); err != nil || !filepath.IsAbs(worktree.LocalPath) {
		return fmt.Errorf("decode frozen worktree path")
	}
	if !isFDLExternalRoot(d.cfg.FDLRunRoot, worktree.LocalPath) {
		return fmt.Errorf("FDL run root must be outside the frozen worktree")
	}
	stateDir := d.fdlExecutorStateDir(run.ID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create FDL executor state directory: %w", err)
	}
	if err := writeFDLExecutorJSON(filepath.Join(stateDir, "worktree.json"), fdlExecutorWorktree{SchemaVersion: 1, LocalPath: worktree.LocalPath}); err != nil {
		return fmt.Errorf("persist frozen FDL worktree binding: %w", err)
	}
	if err := d.persistFDLIssueInput(run.ID, run.IssueSnapshot); err != nil {
		return err
	}

	// The controller config is frozen server-side without a local path. Only
	// this executor derives repo_root from the daemon-bound worktree.
	config := make(map[string]any, len(profile.ControllerConfig)+1)
	for key, value := range profile.ControllerConfig {
		config[key] = value
	}
	config["repo_root"] = worktree.LocalPath
	privateState := filepath.Join(filepath.Dir(d.cfg.FDLRunRoot), ".multica-fdl-executor")
	if err := os.MkdirAll(privateState, 0o700); err != nil {
		return fmt.Errorf("create FDL executor state directory: %w", err)
	}
	configPath := filepath.Join(privateState, run.ID+".config.json")
	configBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("serialize frozen controller config: %w", err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		return fmt.Errorf("write private FDL config: %w", err)
	}
	runRoot := filepath.Join(d.cfg.FDLRunRoot, run.ID)
	if err := d.runFDLCommand(ctx, "validate-config", "--config", configPath); err != nil {
		return err
	}
	fdlRunID, err := d.readOrInitializeFDLRun(ctx, runRoot, configPath)
	if err != nil {
		return err
	}
	if err := d.runFDLCommand(ctx, "preflight-run", "--run-root", runRoot); err != nil {
		return err
	}
	if err := d.client.InitializeFDLIssueRun(ctx, run.FDLRuntimeID, run.ID, fdlRunID); err != nil {
		return fmt.Errorf("bind initialized FDL run: %w", err)
	}
	return d.refreshFDLMailbox(ctx, PendingFDLIssueRun{
		ID: run.ID, IssueID: run.IssueID, FDLRunID: &fdlRunID, Status: "running", Phase: "setup", FDLRuntimeID: run.FDLRuntimeID,
	})
}

// refreshFDLMailbox holds a local filesystem lease while it reads the current
// Controller action. It never derives an action from history, and it does not
// issue a second drive while an action is already durable in the mailbox.
func (d *Daemon) refreshFDLMailbox(ctx context.Context, run PendingFDLIssueRun) error {
	if run.Status == "cancelling" && (run.FDLRunID == nil || strings.TrimSpace(*run.FDLRunID) == "") {
		return d.client.CancelFDLIssueRun(ctx, run.FDLRuntimeID, run.ID, "Issue cancelled before FDL Controller initialization")
	}
	if run.FDLRunID == nil || strings.TrimSpace(*run.FDLRunID) == "" {
		return fmt.Errorf("active FDL run has no external run ID")
	}
	stateDir := d.fdlExecutorStateDir(run.ID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create FDL executor state directory: %w", err)
	}
	if err := d.ensureFDLIssueInput(run); err != nil {
		return err
	}
	holderID := fmt.Sprintf("daemon:%s:%d", d.cfg.DaemonID, os.Getpid())
	return withFDLExecutorLease(stateDir, holderID, func(lease *fdlExecutorLease) error {
		if run.Status == "cancelling" {
			return d.terminateFDLRun(ctx, run)
		}
		mailbox, err := readFDLExecutorMailbox(filepath.Join(stateDir, "executor.mailbox"))
		if err != nil {
			return err
		}
		if err := d.recoverFDLDispatchReceipt(ctx, run.ID, run.FDLRuntimeID, mailbox); err != nil {
			return err
		}
		// Dispatch recovery can atomically replace the mailbox after the last
		// activation. Re-read before checking a decision receipt.
		mailbox, err = readFDLExecutorMailbox(filepath.Join(stateDir, "executor.mailbox"))
		if err != nil {
			return err
		}
		if err := d.recoverFDLDecisionReceipt(ctx, run.ID, run.FDLRuntimeID, mailbox); err != nil {
			return err
		}
		mailbox, err = readFDLExecutorMailbox(filepath.Join(stateDir, "executor.mailbox"))
		if err != nil {
			return err
		}
		if err := d.recoverFDLRecoveryReceipt(ctx, run.ID, run.FDLRuntimeID, mailbox); err != nil {
			return err
		}
		// Decision recovery can advance the Controller action after its durable
		// receipt is acknowledged. Re-read before consuming the mailbox again.
		mailbox, err = readFDLExecutorMailbox(filepath.Join(stateDir, "executor.mailbox"))
		if err != nil {
			return err
		}
		if mailbox != nil && mailbox.FDLRunID == *run.FDLRunID && mailbox.ActionID != "" {
			return d.processFDLMailbox(ctx, run, mailbox)
		}
		runRoot := filepath.Join(d.cfg.FDLRunRoot, run.ID)
		operationID, err := newFDLOperationID()
		if err != nil {
			return err
		}
		output, err := d.runFDLCommandOutput(ctx, "drive-run", "--run-root", runRoot, "--operation-id", operationID)
		if err != nil {
			return err
		}
		var envelope struct {
			RunID      string `json:"run_id"`
			NextAction struct {
				ActionID string `json:"action_id"`
				Kind     string `json:"kind"`
			} `json:"next_action"`
		}
		if err := json.Unmarshal(output, &envelope); err != nil || envelope.RunID != *run.FDLRunID || envelope.NextAction.ActionID == "" || envelope.NextAction.Kind == "" {
			return fmt.Errorf("decode current FDL Controller envelope")
		}
		mailbox = &fdlExecutorMailbox{
			SchemaVersion: 1, FDLRunID: envelope.RunID, ActionID: envelope.NextAction.ActionID,
			ActionKind: envelope.NextAction.Kind, Envelope: output, UpdatedAt: time.Now().UTC(),
		}
		if err := writeFDLExecutorJSON(filepath.Join(stateDir, "executor.mailbox"), mailbox); err != nil {
			return err
		}
		lease.ActionID = mailbox.ActionID
		if err := writeFDLExecutorJSON(filepath.Join(stateDir, "executor.lease"), lease); err != nil {
			return err
		}
		return d.processFDLMailbox(ctx, run, mailbox)
	})
}

func (d *Daemon) processFDLMailbox(ctx context.Context, run PendingFDLIssueRun, mailbox *fdlExecutorMailbox) error {
	if run.FDLRunID == nil {
		return fmt.Errorf("FDL mailbox has no Controller run ID")
	}
	switch mailbox.ActionKind {
	case "request_human_decision":
		return d.processFDLHumanDecision(ctx, run, mailbox)
	case "recover_external_work":
		return d.processFDLExternalRecovery(ctx, run, mailbox)
	case "handoff_external":
		return d.processFDLHandoff(ctx, run, mailbox)
	case "stop":
		return d.projectFDLStop(ctx, run, mailbox)
	}
	if strings.HasPrefix(mailbox.ActionKind, "dispatch_") {
		if err := d.dispatchFDLMailbox(ctx, run.ID, run.FDLRuntimeID, mailbox); err != nil {
			return err
		}
	}
	if err := d.collectFDLTaskResults(ctx, run.ID, *run.FDLRunID); err != nil {
		return err
	}
	return d.client.UpdateFDLIssueRunProjection(ctx, run.FDLRuntimeID, run.ID, FDLProjection{
		Status: "running", Phase: fdlProjectionPhase(run.Phase),
		Summary: "Controller action is held by the local executor", ActionType: mailbox.ActionKind,
	})
}

// terminateFDLRun is the reverse half of the FDL boundary. The server has
// already cancelled direct Agent tasks; this durable receipt ensures the local
// Controller receives exactly one replay-safe terminal command after restart.
func (d *Daemon) terminateFDLRun(ctx context.Context, run PendingFDLIssueRun) error {
	if run.FDLRunID == nil || *run.FDLRunID == "" {
		return fmt.Errorf("cancelling FDL run has no Controller run ID")
	}
	stateDir := d.fdlExecutorStateDir(run.ID)
	receiptPath := filepath.Join(stateDir, "cancellation-receipt.json")
	var receipt fdlCancellationReceipt
	found, err := readFDLExecutorJSON(receiptPath, &receipt)
	if err != nil {
		return err
	}
	if found && (receipt.SchemaVersion != 1 || receipt.FDLRunID != *run.FDLRunID || receipt.OperationID == "") {
		return fmt.Errorf("FDL cancellation receipt conflicts with run")
	}
	if !found {
		op, err := newFDLOperationID()
		if err != nil {
			return err
		}
		receipt = fdlCancellationReceipt{SchemaVersion: 1, FDLRunID: *run.FDLRunID, OperationID: op, UpdatedAt: time.Now().UTC()}
		if err := writeFDLExecutorJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	if !receipt.Terminated {
		if err := d.runFDLCommand(ctx, "terminate-run", "--run-root", filepath.Join(d.cfg.FDLRunRoot, run.ID), "--status", "cancelled", "--reason", "Issue cancelled in Multica", "--operation-id", receipt.OperationID); err != nil {
			return fmt.Errorf("terminate FDL Controller run: %w", err)
		}
		receipt.Terminated = true
		receipt.UpdatedAt = time.Now().UTC()
		if err := writeFDLExecutorJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	return d.client.CancelFDLIssueRun(ctx, run.FDLRuntimeID, run.ID, "Issue cancelled; Controller terminated and direct tasks cancelled")
}

func (d *Daemon) readOrInitializeFDLRun(ctx context.Context, runRoot, configPath string) (string, error) {
	statePath := filepath.Join(runRoot, "state.json")
	if raw, err := os.ReadFile(statePath); err == nil {
		var state struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal(raw, &state) == nil && state.RunID != "" {
			return state.RunID, nil
		}
		return "", fmt.Errorf("existing FDL run state has no run_id")
	}
	output, err := d.runFDLCommandOutput(ctx, "init-run", "--config", configPath, "--run-root", runRoot)
	if err != nil {
		return "", err
	}
	var initialized struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(output, &initialized); err != nil || initialized.RunID == "" {
		return "", fmt.Errorf("read FDL init result")
	}
	return initialized.RunID, nil
}

func (d *Daemon) runFDLCommand(ctx context.Context, args ...string) error {
	_, err := d.runFDLCommandOutput(ctx, args...)
	return err
}

func (d *Daemon) runFDLCommandOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.cfg.FDLCLIPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("FDL %s: %w: %s", args[0], err, boundedFDLErrorText(string(output)))
	}
	return output, nil
}

func isFDLExternalRoot(runRoot, repoRoot string) bool {
	fromRepo, err := filepath.Rel(repoRoot, runRoot)
	if err != nil || (fromRepo != ".." && !strings.HasPrefix(fromRepo, ".."+string(filepath.Separator))) {
		return false
	}
	fromRun, err := filepath.Rel(runRoot, repoRoot)
	return err == nil && (fromRun == ".." || strings.HasPrefix(fromRun, ".."+string(filepath.Separator)))
}

func (d *Daemon) fdlExecutorStateDir(runID string) string {
	return filepath.Join(filepath.Dir(d.cfg.FDLRunRoot), ".multica-fdl-executor", "runs", runID)
}

func withFDLExecutorLease(stateDir, holderID string, fn func(*fdlExecutorLease) error) error {
	lockPath := filepath.Join(stateDir, "executor.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open FDL executor lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("FDL executor is already active")
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	leasePath := filepath.Join(stateDir, "executor.lease")
	lease := &fdlExecutorLease{SchemaVersion: 1, HolderID: holderID, ExpiresAt: time.Now().UTC().Add(fdlExecutorLeaseTTL)}
	if existing, err := readFDLExecutorLease(leasePath); err != nil {
		return err
	} else if existing != nil && existing.ExpiresAt.After(time.Now().UTC()) && existing.HolderID != holderID {
		return fmt.Errorf("FDL executor lease belongs to another holder")
	} else if existing != nil {
		lease = existing
		lease.HolderID = holderID
		lease.ExpiresAt = time.Now().UTC().Add(fdlExecutorLeaseTTL)
	}
	if err := writeFDLExecutorJSON(leasePath, lease); err != nil {
		return err
	}
	return fn(lease)
}

func readFDLExecutorLease(path string) (*fdlExecutorLease, error) {
	var lease fdlExecutorLease
	found, err := readFDLExecutorJSON(path, &lease)
	if err != nil || !found {
		return nil, err
	}
	if lease.SchemaVersion != 1 || lease.HolderID == "" || lease.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("invalid FDL executor lease")
	}
	return &lease, nil
}

func readFDLExecutorMailbox(path string) (*fdlExecutorMailbox, error) {
	var mailbox fdlExecutorMailbox
	found, err := readFDLExecutorJSON(path, &mailbox)
	if err != nil || !found {
		return nil, err
	}
	if mailbox.SchemaVersion != 1 || mailbox.FDLRunID == "" || mailbox.ActionID == "" || mailbox.ActionKind == "" || len(mailbox.Envelope) == 0 {
		return nil, fmt.Errorf("invalid FDL executor mailbox")
	}
	return &mailbox, nil
}

func readFDLExecutorJSON(path string, value any) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read FDL executor state: %w", err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return false, fmt.Errorf("decode FDL executor state: %w", err)
	}
	return true, nil
}

func writeFDLExecutorJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode FDL executor state: %w", err)
	}
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return fmt.Errorf("write FDL executor state: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("commit FDL executor state: %w", err)
	}
	return nil
}

func newFDLOperationID() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate FDL operation ID: %w", err)
	}
	return fmt.Sprintf("multica-%x", random), nil
}

func fdlProjectionPhase(phase string) string {
	switch phase {
	case "intake", "pre_design", "design", "implementation", "review", "handoff":
		return phase
	case "planning":
		return "design"
	default:
		return "setup"
	}
}

type fdlDispatchEnvelope struct {
	RunID      string `json:"run_id"`
	NextAction struct {
		ActionID       string `json:"action_id"`
		Kind           string `json:"kind"`
		ExternalWorkID string `json:"external_work_id"`
		WorkItems      []struct {
			WorkItemID      string `json:"work_item_id"`
			SubmissionToken string `json:"submission_token"`
			Dispatch        struct {
				Path   string `json:"path"`
				SHA256 string `json:"sha256"`
			} `json:"dispatch"`
		} `json:"work_items"`
	} `json:"next_action"`
}

type fdlDispatchPayload struct {
	Attempt struct {
		Role                string `json:"role"`
		Phase               string `json:"phase"`
		AttemptID           string `json:"attempt_id"`
		ControlManifestHash string `json:"control_manifest_hash"`
	} `json:"attempt"`
	Instructions  json.RawMessage            `json:"instructions"`
	InputHashes   map[string]string          `json:"input_hashes"`
	InputEvidence map[string]json.RawMessage `json:"input_evidence"`
}

// dispatchFDLMailbox is the durable bridge for one Controller dispatch action:
// all local task rows are created before the one batch acknowledgement, and no
// task becomes claimable before that acknowledgement is durably accepted.
func (d *Daemon) dispatchFDLMailbox(ctx context.Context, runID, runtimeID string, mailbox *fdlExecutorMailbox) error {
	bindings, err := d.materializeFDLDispatch(runID, mailbox)
	if err != nil {
		return err
	}
	if err := d.createFDLDispatchTasks(ctx, runID, runtimeID, bindings); err != nil {
		return err
	}
	return d.acknowledgeAndActivateFDLDispatch(ctx, runID, runtimeID, mailbox)
}

func (d *Daemon) fdlBindingPath(runID, workItemID string) string {
	return filepath.Join(d.fdlExecutorStateDir(runID), "work-items", workItemID, "binding.json")
}

func (d *Daemon) readFDLDispatchPayload(runID, workItemID string) (fdlDispatchPayload, error) {
	var payload fdlDispatchPayload
	data, err := os.ReadFile(filepath.Join(d.fdlExecutorStateDir(runID), "work-items", workItemID, "dispatch.json"))
	if err != nil {
		return payload, fmt.Errorf("read private FDL dispatch: %w", err)
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Attempt.Role == "" {
		return payload, fmt.Errorf("decode private FDL dispatch")
	}
	return payload, nil
}

// fdlProfileRole translates Controller role names and isolated review lanes to
// the frozen Delivery Profile vocabulary. Unknown roles fail closed rather than
// accidentally invoking a nearby but incorrect Agent.
func fdlProfileRole(payload fdlDispatchPayload) (string, error) {
	switch payload.Attempt.Role {
	case "feature-delivery-intake":
		return "intake", nil
	case "feature-delivery-planner":
		return "planner", nil
	case "feature-delivery-implementer":
		return "implementer", nil
	case "explorer":
		return "explorer:impact_analysis", nil
	case "feature-delivery-reviewer":
		var instructions struct {
			Lane string `json:"lane"`
		}
		if json.Unmarshal(payload.Instructions, &instructions) != nil {
			return "", fmt.Errorf("decode review lane")
		}
		switch instructions.Lane {
		case "correctness", "conflict_recheck":
			return "reviewer:correctness", nil
		case "regression":
			return "reviewer:regression", nil
		case "specialist":
			return "reviewer:specialist", nil
		default:
			return "", fmt.Errorf("unsupported FDL review lane %q", instructions.Lane)
		}
	default:
		return "", fmt.Errorf("unsupported FDL Controller role %q", payload.Attempt.Role)
	}
}

func (d *Daemon) createFDLDispatchTasks(ctx context.Context, runID, runtimeID string, bindings []fdlWorkItemBinding) error {
	for _, binding := range bindings {
		if binding.State == "dispatch_failed" || binding.TaskID != "" {
			continue
		}
		payload, err := d.readFDLDispatchPayload(runID, binding.WorkItemID)
		if err != nil {
			return err
		}
		role, err := fdlProfileRole(payload)
		if err != nil {
			binding.State = "dispatch_failed"
			binding.DispatchError = boundedFDLError(err)
			binding.UpdatedAt = time.Now().UTC()
			if err := writeFDLExecutorJSON(d.fdlBindingPath(runID, binding.WorkItemID), binding); err != nil {
				return err
			}
			continue
		}
		binding.Role = role
		instructions, err := d.fdlAgentInstructions(runID, binding, payload)
		if err != nil {
			return err
		}
		executionWorkspace, err := d.materializeFDLExecutionWorkspace(runID, binding, payload)
		if err != nil {
			binding.State = "dispatch_failed"
			binding.DispatchError = boundedFDLError(err)
			binding.UpdatedAt = time.Now().UTC()
			if writeErr := writeFDLExecutorJSON(d.fdlBindingPath(runID, binding.WorkItemID), binding); writeErr != nil {
				return writeErr
			}
			continue
		}
		taskID, err := d.client.CreateFDLAgentTask(ctx, runtimeID, runID, role, instructions, binding.DispatchKey, 0, executionWorkspace)
		if err != nil {
			// A rejected frozen runtime/role cannot become valid through a blind
			// retry. Report it as a declared dispatch failure; transport errors
			// stay unacknowledged and are retried idempotently by dispatch_key.
			var requestErr *requestError
			if errors.As(err, &requestErr) && requestErr.StatusCode >= 400 && requestErr.StatusCode < 500 {
				binding.State = "dispatch_failed"
				binding.DispatchError = boundedFDLError(err)
				binding.UpdatedAt = time.Now().UTC()
				if writeErr := writeFDLExecutorJSON(d.fdlBindingPath(runID, binding.WorkItemID), binding); writeErr != nil {
					return writeErr
				}
				continue
			}
			return fmt.Errorf("create direct FDL task for %s: %w", binding.WorkItemID, err)
		}
		binding.TaskID = taskID
		binding.State = "pending_ack"
		binding.UpdatedAt = time.Now().UTC()
		if err := writeFDLExecutorJSON(d.fdlBindingPath(runID, binding.WorkItemID), binding); err != nil {
			return err
		}
	}
	return nil
}

// fdlAgentInstructions deliberately shares only Controller instructions and
// the caller's own private result location. It never sends a dispatch file,
// run-root location, external-work ID, or submission token to an Agent.
func (d *Daemon) fdlAgentInstructions(runID string, binding fdlWorkItemBinding, payload fdlDispatchPayload) (string, error) {
	if len(payload.Instructions) == 0 || !json.Valid(payload.Instructions) {
		return "", fmt.Errorf("FDL dispatch instructions are invalid")
	}
	prettyInstructions := &bytes.Buffer{}
	if err := json.Indent(prettyInstructions, payload.Instructions, "", "  "); err != nil {
		return "", fmt.Errorf("format FDL dispatch instructions: %w", err)
	}
	itemDir := filepath.Join(d.fdlExecutorStateDir(runID), "work-items", binding.WorkItemID)
	if binding.Role == "explorer:impact_analysis" {
		return fmt.Sprintf(`You are the frozen FDL Explorer role.

Execute only this Controller instruction payload:
%s

Work only in your assigned read-only source projection. Do not inspect or write any FDL run root, Controller state, receipts, other work-item directories, or external systems. Do not create sub-agents, use Issue comments for coordination, or expose delivery tokens.

Write exactly one advisory JSON object to:
%s
The object must include schema_version=1 and request_id=%q, plus the requested evidence fields.
`, prettyInstructions.String(), filepath.Join(itemDir, "advisory.json"), binding.WorkItemID), nil
	}
	issueInput, err := d.readFDLIssueInput(runID)
	if err != nil {
		return "", err
	}
	issueInputJSON, err := json.Marshal(issueInput)
	if err != nil {
		return "", fmt.Errorf("encode frozen FDL Issue input: %w", err)
	}
	prettyIssueInput := &bytes.Buffer{}
	if err := json.Indent(prettyIssueInput, issueInputJSON, "", "  "); err != nil {
		return "", fmt.Errorf("format frozen FDL Issue input: %w", err)
	}
	contractInstruction := ""
	if binding.Role == "planner" {
		contractInstruction = `
For the compact delivery plan, meta.json must include a contract_index object with this exact shape:
{"schema_version":1,"artifact_kind":"delivery_plan","acceptance_criteria":[{"key":"AC-...","statement":"...","supersedes":null}],"acceptance_refs":[],"invariants":[{"key":"INV-...","statement":"...","acceptance_keys":["AC-..."],"supersedes":null}]}
Use stable namespaced keys such as AC-CORE-001 and INV-CORE-001; keys like AC-001 are invalid. Your Markdown delivery plan must explicitly cite every AC-/INV- key declared in contract_index, and must not introduce an uncatalogued acceptance criterion or invariant. Do not put a context_pack in meta.json: the local executor binds its private Controller hashes.
`
	}
	consistencyInstruction := `If you write metadata, set evidence_consistency=not_applicable unless the Controller instruction exposes Context Pack evidence.`
	if fdlRequiresEvidenceConsistency(payload) {
		consistencyInstruction = `You MUST write meta.json and set evidence_consistency=checked after comparing the exposed Context Pack evidence with your report. If you find a conflict, set evidence_consistency=conflict_found and provide the required blocker or review finding; never use not_applicable for this work item.`
	}
	changeInstruction := ""
	if binding.Role == "implementer" {
		changeInstruction = `Do not set changed_paths_from_workspace in meta.json. The executor derives the actual changed paths from your frozen workspace after you finish.`
	}
	instructions := fmt.Sprintf(`You are the frozen FDL delivery role %q.

Execute only this Controller instruction payload:
%s

Frozen Multica Issue input (requirements seed, not a Controller token or task binding):
%s
Use it to produce or implement the accepted delivery contract. The Controller-managed planning artifacts and their accepted contract keys remain authoritative after planning.

Work only in your assigned task workspace and within the frozen change rules. Do not inspect or write any FDL run root, Controller state, receipts, other work-item directories, or external systems. Do not create sub-agents, use Issue comments for coordination, or expose delivery tokens.

Write the role report in Markdown to:
%s
Write JSON metadata to %s only when required above or when you need to report a non-default outcome. It may contain only outcome (completed|blocked|failed), evidence_consistency (checked|not_applicable|conflict_found), contract_index, context_pack, blockers, and failure. Only a Reviewer may additionally set decision (accepted|changes_requested), findings, or finding_resolutions. All other roles must omit those review-only fields; place ordinary observations in the Markdown report and use blockers only when the work is blocked. Do not include identifiers copied from FDL state; the local executor binds them.

%s
%s
%s`, binding.Role, prettyInstructions.String(), prettyIssueInput.String(), filepath.Join(itemDir, "report.md"), filepath.Join(itemDir, "meta.json"), consistencyInstruction, changeInstruction, contractInstruction)
	if len(instructions) > 20000 {
		return "", fmt.Errorf("FDL Agent instructions exceed the direct-task limit")
	}
	return instructions, nil
}

func fdlRequiresEvidenceConsistency(payload fdlDispatchPayload) bool {
	if payload.Attempt.Phase == "planning" || payload.Attempt.Phase == "design" {
		return true
	}
	_, ok := payload.InputHashes["context_pack_hash"]
	return ok
}

func fdlExtractsWorkspaceChanges(payload fdlDispatchPayload) bool {
	return payload.Attempt.Phase == "implementation"
}

func (d *Daemon) acknowledgeAndActivateFDLDispatch(ctx context.Context, runID, runtimeID string, mailbox *fdlExecutorMailbox) error {
	stateDir := d.fdlExecutorStateDir(runID)
	receiptPath := filepath.Join(stateDir, "dispatch-receipt.json")
	receipt, err := d.loadOrCreateFDLDispatchReceipt(runID, mailbox)
	if err != nil {
		return err
	}
	if !receipt.ControllerAcknowledged {
		eventPath := filepath.Join(stateDir, "ack-event.json")
		if err := writeFDLExecutorJSON(eventPath, d.fdlAcknowledgementEvent(receipt)); err != nil {
			return err
		}
		output, err := d.runFDLCommandOutput(ctx, "drive-run", "--run-root", filepath.Join(d.cfg.FDLRunRoot, runID), "--operation-id", receipt.AckOperationID, "--event", eventPath)
		if err != nil {
			return fmt.Errorf("acknowledge FDL dispatch: %w", err)
		}
		if err := validateFDLReturnedEnvelope(output, receipt.FDLRunID); err != nil {
			return err
		}
		receipt.ControllerAcknowledged = true
		receipt.ReturnedEnvelope = output
		receipt.UpdatedAt = time.Now().UTC()
		if err := writeFDLExecutorJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	if err := d.activateFDLDispatchTasks(ctx, runID, runtimeID, receipt); err != nil {
		return err
	}
	if err := writeFDLExecutorJSON(filepath.Join(stateDir, "executor.mailbox"), fdlMailboxFromEnvelope(receipt.ReturnedEnvelope)); err != nil {
		return err
	}
	if err := os.Remove(receiptPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove completed FDL dispatch receipt: %w", err)
	}
	if err := os.Remove(filepath.Join(stateDir, "ack-event.json")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove completed FDL acknowledgement event: %w", err)
	}
	return nil
}

func (d *Daemon) loadOrCreateFDLDispatchReceipt(runID string, mailbox *fdlExecutorMailbox) (*fdlDispatchReceipt, error) {
	path := filepath.Join(d.fdlExecutorStateDir(runID), "dispatch-receipt.json")
	var receipt fdlDispatchReceipt
	found, err := readFDLExecutorJSON(path, &receipt)
	if err != nil {
		return nil, err
	}
	if found {
		if err := validateFDLDispatchReceipt(receipt, mailbox); err != nil {
			return nil, err
		}
		return &receipt, nil
	}
	bindings, err := d.materializeFDLDispatch(runID, mailbox)
	if err != nil {
		return nil, err
	}
	operationID, err := newFDLOperationID()
	if err != nil {
		return nil, err
	}
	receipt = fdlDispatchReceipt{
		SchemaVersion: 1, FDLRunID: mailbox.FDLRunID, ActionID: mailbox.ActionID,
		AckOperationID: operationID, TaskIDs: map[string]string{}, DispatchFailures: map[string]string{}, Activated: map[string]bool{}, UpdatedAt: time.Now().UTC(),
	}
	for _, binding := range bindings {
		if receipt.ExternalWorkID == "" {
			receipt.ExternalWorkID = binding.ExternalWorkID
		} else if receipt.ExternalWorkID != binding.ExternalWorkID {
			return nil, fmt.Errorf("FDL dispatch work items disagree on external work ID")
		}
		if binding.State == "dispatch_failed" {
			receipt.DispatchFailures[binding.WorkItemID] = binding.DispatchError
			continue
		}
		if binding.TaskID == "" || binding.State != "pending_ack" {
			return nil, fmt.Errorf("FDL work item %s is not ready for acknowledgement", binding.WorkItemID)
		}
		receipt.TaskIDs[binding.WorkItemID] = binding.TaskID
	}
	if receipt.ExternalWorkID == "" || len(receipt.TaskIDs)+len(receipt.DispatchFailures) != len(bindings) {
		return nil, fmt.Errorf("FDL dispatch receipt does not cover every work item")
	}
	if err := writeFDLExecutorJSON(path, receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func validateFDLDispatchReceipt(receipt fdlDispatchReceipt, mailbox *fdlExecutorMailbox) error {
	if receipt.SchemaVersion != 1 || receipt.FDLRunID != mailbox.FDLRunID || receipt.ActionID != mailbox.ActionID || receipt.ExternalWorkID == "" || receipt.AckOperationID == "" || receipt.TaskIDs == nil || receipt.Activated == nil {
		return fmt.Errorf("FDL dispatch receipt conflicts with mailbox")
	}
	return nil
}

func (d *Daemon) fdlAcknowledgementEvent(receipt *fdlDispatchReceipt) map[string]any {
	ids := make([]string, 0, len(receipt.TaskIDs)+len(receipt.DispatchFailures))
	for id := range receipt.TaskIDs {
		ids = append(ids, id)
	}
	for id := range receipt.DispatchFailures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	acknowledgements := make([]map[string]string, 0, len(ids))
	executorIdentity := "multica-daemon:" + d.cfg.DaemonID
	for _, id := range ids {
		status := "acknowledged"
		if _, failed := receipt.DispatchFailures[id]; failed {
			status = "dispatch_failed"
		}
		acknowledgements = append(acknowledgements, map[string]string{
			"work_item_id": id, "status": status, "executor_identity": executorIdentity, "identity_assurance": "human_attested",
		})
	}
	return map[string]any{
		"envelope_version": 1, "kind": "external_work_acknowledged", "run_id": receipt.FDLRunID,
		"action_id": receipt.ActionID, "external_work_id": receipt.ExternalWorkID, "acknowledgements": acknowledgements,
	}
}

func (d *Daemon) activateFDLDispatchTasks(ctx context.Context, runID, runtimeID string, receipt *fdlDispatchReceipt) error {
	workItemIDs := make([]string, 0, len(receipt.TaskIDs))
	for workItemID := range receipt.TaskIDs {
		workItemIDs = append(workItemIDs, workItemID)
	}
	sort.Strings(workItemIDs)
	for _, workItemID := range workItemIDs {
		if receipt.Activated[workItemID] {
			continue
		}
		if err := d.client.ActivateFDLAgentTask(ctx, runtimeID, runID, receipt.TaskIDs[workItemID]); err != nil {
			return fmt.Errorf("activate FDL task for %s: %w", workItemID, err)
		}
		var binding fdlWorkItemBinding
		if found, err := readFDLExecutorJSON(d.fdlBindingPath(runID, workItemID), &binding); err != nil {
			return fmt.Errorf("read FDL binding for activation: %w", err)
		} else if !found {
			return fmt.Errorf("FDL binding for activation is missing")
		}
		binding.State = "activated"
		binding.UpdatedAt = time.Now().UTC()
		if err := writeFDLExecutorJSON(d.fdlBindingPath(runID, workItemID), binding); err != nil {
			return err
		}
		receipt.Activated[workItemID] = true
		receipt.UpdatedAt = time.Now().UTC()
		if err := writeFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "dispatch-receipt.json"), receipt); err != nil {
			return err
		}
	}
	return nil
}

// recoverFDLDispatchReceipt finishes only work whose Controller acknowledgement
// is already durable. It must run before reading a mailbox action so an old
// dispatch can never be driven a second time after a daemon crash.
func (d *Daemon) recoverFDLDispatchReceipt(ctx context.Context, runID, runtimeID string, mailbox *fdlExecutorMailbox) error {
	path := filepath.Join(d.fdlExecutorStateDir(runID), "dispatch-receipt.json")
	var receipt fdlDispatchReceipt
	found, err := readFDLExecutorJSON(path, &receipt)
	if err != nil || !found {
		return err
	}
	if !receipt.ControllerAcknowledged {
		if mailbox == nil || validateFDLDispatchReceipt(receipt, mailbox) != nil {
			return fmt.Errorf("unacknowledged FDL dispatch receipt does not match mailbox")
		}
		return nil
	}
	if err := validateFDLReturnedEnvelope(receipt.ReturnedEnvelope, receipt.FDLRunID); err != nil {
		return err
	}
	returned := fdlMailboxFromEnvelope(receipt.ReturnedEnvelope)
	if mailbox != nil && mailbox.ActionID == returned.ActionID {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if mailbox == nil || validateFDLDispatchReceipt(receipt, mailbox) != nil {
		return fmt.Errorf("acknowledged FDL dispatch receipt conflicts with mailbox")
	}
	if err := d.activateFDLDispatchTasks(ctx, runID, runtimeID, &receipt); err != nil {
		return err
	}
	if err := writeFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "executor.mailbox"), returned); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func validateFDLReturnedEnvelope(raw json.RawMessage, runID string) error {
	mailbox := fdlMailboxFromEnvelope(raw)
	if mailbox.FDLRunID != runID || mailbox.ActionID == "" || mailbox.ActionKind == "" {
		return fmt.Errorf("decode FDL Controller acknowledgement envelope")
	}
	return nil
}

func fdlMailboxFromEnvelope(raw json.RawMessage) *fdlExecutorMailbox {
	var envelope struct {
		RunID      string `json:"run_id"`
		NextAction struct {
			ActionID string `json:"action_id"`
			Kind     string `json:"kind"`
		} `json:"next_action"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return &fdlExecutorMailbox{}
	}
	return &fdlExecutorMailbox{SchemaVersion: 1, FDLRunID: envelope.RunID, ActionID: envelope.NextAction.ActionID, ActionKind: envelope.NextAction.Kind, Envelope: raw, UpdatedAt: time.Now().UTC()}
}

type fdlHumanDecisionAction struct {
	Kind  string `json:"kind"`
	Phase string `json:"phase"`
}

func fdlMailboxDecision(mailbox *fdlExecutorMailbox) (fdlHumanDecisionAction, error) {
	var envelope struct {
		RunID      string `json:"run_id"`
		NextAction struct {
			ActionID string                 `json:"action_id"`
			Kind     string                 `json:"kind"`
			Decision fdlHumanDecisionAction `json:"decision"`
		} `json:"next_action"`
	}
	if json.Unmarshal(mailbox.Envelope, &envelope) != nil || envelope.RunID != mailbox.FDLRunID || envelope.NextAction.ActionID != mailbox.ActionID || envelope.NextAction.Kind != "request_human_decision" || envelope.NextAction.Decision.Kind == "" {
		return fdlHumanDecisionAction{}, fmt.Errorf("decode FDL human decision action")
	}
	return envelope.NextAction.Decision, nil
}

func fdlDecisionChoices(kind string) ([]string, bool) {
	switch kind {
	case "planning_checkpoint":
		return []string{"accept", "reject", "retry", "cancel"}, true
	case "final_approval":
		return []string{"accept", "approve", "reject", "cancel"}, true
	case "review_changes":
		return []string{"accept", "retry", "reject", "cancel"}, true
	case "environment_preflight_blocked":
		return []string{"retry", "cancel"}, true
	case "role_not_completed", "gate_failure", "code_snapshot_drift":
		return []string{"accept", "retry", "reject", "cancel"}, true
	default:
		return nil, false
	}
}

func fdlDecisionPhase(kind, controllerPhase string) string {
	switch kind {
	case "planning_checkpoint":
		if controllerPhase == "intake" || controllerPhase == "design" {
			return controllerPhase
		}
		return "design"
	case "final_approval", "review_changes":
		return "review"
	case "gate_failure":
		return "gate"
	case "environment_preflight_blocked":
		return "pre_design"
	default:
		return "implementation"
	}
}

func (d *Daemon) decisionReceiptPath(runID string) string {
	return filepath.Join(d.fdlExecutorStateDir(runID), "decision-receipt.json")
}

func (d *Daemon) processFDLHumanDecision(ctx context.Context, run PendingFDLIssueRun, mailbox *fdlExecutorMailbox) error {
	action, err := fdlMailboxDecision(mailbox)
	if err != nil {
		return err
	}
	choices, ok := fdlDecisionChoices(action.Kind)
	if !ok {
		return fmt.Errorf("unsupported FDL human decision kind %q", action.Kind)
	}
	if err := d.client.UpdateFDLIssueRunProjection(ctx, run.FDLRuntimeID, run.ID, FDLProjection{
		Status: "awaiting_human_decision", Phase: fdlDecisionPhase(action.Kind, action.Phase),
		WaitingReason: "A structured FDL decision is required", ActionType: mailbox.ActionKind,
		DecisionActionID: mailbox.ActionID, DecisionKind: action.Kind, AllowedDecisions: choices,
	}); err != nil {
		return err
	}
	decision, err := d.client.GetFDLHumanDecision(ctx, run.FDLRuntimeID, run.ID, mailbox.ActionID)
	if err != nil {
		var requestErr *requestError
		if errors.As(err, &requestErr) && requestErr.StatusCode == 404 {
			return nil
		}
		return fmt.Errorf("load FDL human decision: %w", err)
	}
	if decision.ActionID != mailbox.ActionID || !fdlDecisionAllowed(choices, decision.Decision) {
		return fmt.Errorf("FDL human decision does not match current Controller action")
	}
	return d.submitFDLHumanDecision(ctx, run.FDLRuntimeID, run.ID, mailbox, decision)
}

type fdlRecoveryAction struct {
	ExternalWorkID string   `json:"external_work_id"`
	WorkItems      []string `json:"work_items"`
}

func fdlMailboxRecovery(mailbox *fdlExecutorMailbox) (fdlRecoveryAction, error) {
	var envelope struct {
		RunID      string `json:"run_id"`
		NextAction struct {
			ActionID string `json:"action_id"`
			Kind     string `json:"kind"`
			// The Controller keeps recovery item identities in the action. They
			// remain local because this envelope never leaves the executor.
			ExternalWorkID string   `json:"external_work_id"`
			WorkItems      []string `json:"work_items"`
		} `json:"next_action"`
	}
	if json.Unmarshal(mailbox.Envelope, &envelope) != nil || envelope.RunID != mailbox.FDLRunID || envelope.NextAction.ActionID != mailbox.ActionID || envelope.NextAction.Kind != "recover_external_work" || envelope.NextAction.ExternalWorkID == "" || len(envelope.NextAction.WorkItems) == 0 {
		return fdlRecoveryAction{}, fmt.Errorf("decode FDL recovery action")
	}
	return fdlRecoveryAction{ExternalWorkID: envelope.NextAction.ExternalWorkID, WorkItems: envelope.NextAction.WorkItems}, nil
}

func (d *Daemon) recoveryReceiptPath(runID string) string {
	return filepath.Join(d.fdlExecutorStateDir(runID), "recovery-receipt.json")
}

func (d *Daemon) processFDLExternalRecovery(ctx context.Context, run PendingFDLIssueRun, mailbox *fdlExecutorMailbox) error {
	action, err := fdlMailboxRecovery(mailbox)
	if err != nil {
		return err
	}
	workItemID := action.WorkItems[0]
	var binding fdlWorkItemBinding
	found, err := readFDLExecutorJSON(d.fdlBindingPath(run.ID, workItemID), &binding)
	if err != nil || !found {
		return fmt.Errorf("read failed FDL work-item binding for recovery")
	}
	if binding.FDLRunID != mailbox.FDLRunID || binding.ExternalWorkID != action.ExternalWorkID || binding.WorkItemID != workItemID || binding.SubmissionToken == "" {
		return fmt.Errorf("FDL recovery action does not match private work-item binding")
	}
	payload, err := d.readFDLDispatchPayload(run.ID, workItemID)
	if err != nil {
		return err
	}
	if err := d.client.UpdateFDLIssueRunProjection(ctx, run.FDLRuntimeID, run.ID, FDLProjection{
		Status: "recovering", Phase: fdlProjectionPhase(payload.Attempt.Phase),
		WaitingReason: "A failed FDL work item requires an explicit retry or cancellation", ActionType: mailbox.ActionKind,
		DecisionActionID: mailbox.ActionID, DecisionKind: "external_work_recovery", AllowedDecisions: []string{"retry", "cancel"},
	}); err != nil {
		return err
	}
	decision, err := d.client.GetFDLHumanDecision(ctx, run.FDLRuntimeID, run.ID, mailbox.ActionID)
	if err != nil {
		var requestErr *requestError
		if errors.As(err, &requestErr) && requestErr.StatusCode == 404 {
			return nil
		}
		return fmt.Errorf("load FDL recovery resolution: %w", err)
	}
	if decision.ActionID != mailbox.ActionID || (decision.Decision != "retry" && decision.Decision != "cancel") {
		return fmt.Errorf("FDL recovery resolution does not match current Controller action")
	}
	return d.submitFDLRecoveryResolution(ctx, run.FDLRuntimeID, run.ID, mailbox, binding, decision)
}

func (d *Daemon) submitFDLRecoveryResolution(ctx context.Context, runtimeID, runID string, mailbox *fdlExecutorMailbox, binding fdlWorkItemBinding, decision FDLHumanDecision) error {
	receiptPath := d.recoveryReceiptPath(runID)
	var receipt fdlRecoveryReceipt
	found, err := readFDLExecutorJSON(receiptPath, &receipt)
	if err != nil {
		return err
	}
	if found {
		if receipt.SchemaVersion != 1 || receipt.DecisionID != decision.ID || receipt.FDLRunID != mailbox.FDLRunID || receipt.ActionID != mailbox.ActionID || receipt.ExternalWorkID != binding.ExternalWorkID || receipt.WorkItemID != binding.WorkItemID || receipt.SubmissionToken != binding.SubmissionToken || receipt.Resolution != decision.Decision || receipt.OperationID == "" {
			return fmt.Errorf("FDL recovery receipt conflicts with mailbox")
		}
	} else {
		if decision.Status == "pending" {
			operationID, err := newFDLOperationID()
			if err != nil {
				return err
			}
			decision, err = d.client.ClaimFDLHumanDecision(ctx, runtimeID, runID, decision.ID, operationID)
			if err != nil {
				return fmt.Errorf("claim FDL recovery resolution: %w", err)
			}
		}
		if decision.Status != "processing" || decision.OperationID == nil || *decision.OperationID == "" {
			return fmt.Errorf("FDL recovery resolution has no recoverable operation")
		}
		receipt = fdlRecoveryReceipt{
			SchemaVersion: 1, DecisionID: decision.ID, FDLRunID: mailbox.FDLRunID, ActionID: mailbox.ActionID,
			ExternalWorkID: binding.ExternalWorkID, WorkItemID: binding.WorkItemID, SubmissionToken: binding.SubmissionToken,
			Resolution: decision.Decision, OperationID: *decision.OperationID, UpdatedAt: time.Now().UTC(),
		}
		if err := writeFDLExecutorJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	if !receipt.ControllerSubmitted {
		event := map[string]any{
			"envelope_version": 1, "kind": "external_work_abandoned", "run_id": receipt.FDLRunID,
			"external_work_id": receipt.ExternalWorkID, "work_item_id": receipt.WorkItemID,
			"submission_token": receipt.SubmissionToken, "resolution": receipt.Resolution,
			"reason": "Human selected an explicit FDL recovery resolution",
		}
		if receipt.Resolution == "cancel" {
			event["terminal_status"] = "cancelled"
		}
		eventPath := filepath.Join(d.fdlExecutorStateDir(runID), "recovery-event.json")
		if err := writeFDLExecutorJSON(eventPath, event); err != nil {
			return err
		}
		output, err := d.runFDLCommandOutput(ctx, "drive-run", "--run-root", filepath.Join(d.cfg.FDLRunRoot, runID), "--operation-id", receipt.OperationID, "--event", eventPath)
		if err != nil {
			return fmt.Errorf("submit FDL recovery resolution: %w", err)
		}
		if err := validateFDLReturnedEnvelope(output, receipt.FDLRunID); err != nil {
			return err
		}
		receipt.ControllerSubmitted = true
		receipt.ReturnedEnvelope = output
		receipt.UpdatedAt = time.Now().UTC()
		if err := writeFDLExecutorJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	return d.finalizeFDLRecoveryReceipt(ctx, runtimeID, runID, receipt)
}

func (d *Daemon) recoverFDLRecoveryReceipt(ctx context.Context, runID, runtimeID string, mailbox *fdlExecutorMailbox) error {
	var receipt fdlRecoveryReceipt
	found, err := readFDLExecutorJSON(d.recoveryReceiptPath(runID), &receipt)
	if err != nil || !found {
		return err
	}
	if receipt.SchemaVersion != 1 || receipt.DecisionID == "" || receipt.FDLRunID == "" || receipt.ActionID == "" || receipt.ExternalWorkID == "" || receipt.WorkItemID == "" || receipt.SubmissionToken == "" || (receipt.Resolution != "retry" && receipt.Resolution != "cancel") || receipt.OperationID == "" {
		return fmt.Errorf("invalid FDL recovery receipt")
	}
	if !receipt.ControllerSubmitted {
		if mailbox == nil || mailbox.FDLRunID != receipt.FDLRunID || mailbox.ActionID != receipt.ActionID || mailbox.ActionKind != "recover_external_work" {
			return fmt.Errorf("unsubmitted FDL recovery receipt conflicts with mailbox")
		}
		return nil
	}
	if err := validateFDLReturnedEnvelope(receipt.ReturnedEnvelope, receipt.FDLRunID); err != nil {
		return err
	}
	returned := fdlMailboxFromEnvelope(receipt.ReturnedEnvelope)
	if mailbox != nil && mailbox.ActionID != receipt.ActionID && mailbox.ActionID != returned.ActionID {
		return fmt.Errorf("submitted FDL recovery receipt conflicts with mailbox")
	}
	return d.finalizeFDLRecoveryReceipt(ctx, runtimeID, runID, receipt)
}

func (d *Daemon) finalizeFDLRecoveryReceipt(ctx context.Context, runtimeID, runID string, receipt fdlRecoveryReceipt) error {
	if err := d.client.CompleteFDLHumanDecision(ctx, runtimeID, runID, receipt.DecisionID, receipt.OperationID); err != nil {
		return fmt.Errorf("complete FDL recovery resolution: %w", err)
	}
	// A retry can immediately return a new dispatch. Restore the display
	// projection before the next cycle creates its direct task: that endpoint
	// intentionally refuses work while the run is still marked recovering.
	if projection, dispatch, err := fdlRunningProjectionForDispatch(receipt.ReturnedEnvelope); err != nil {
		return err
	} else if dispatch {
		if err := d.client.UpdateFDLIssueRunProjection(ctx, runtimeID, runID, projection); err != nil {
			return fmt.Errorf("restore FDL projection after recovery: %w", err)
		}
	}
	if err := writeFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "executor.mailbox"), fdlMailboxFromEnvelope(receipt.ReturnedEnvelope)); err != nil {
		return err
	}
	if err := os.Remove(d.recoveryReceiptPath(runID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove completed FDL recovery receipt: %w", err)
	}
	if err := os.Remove(filepath.Join(d.fdlExecutorStateDir(runID), "recovery-event.json")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove submitted FDL recovery event: %w", err)
	}
	return nil
}

// fdlRunningProjectionForDispatch derives only display-safe facts from a
// Controller reply. It leaves decisions and terminal actions untouched.
func fdlRunningProjectionForDispatch(raw json.RawMessage) (FDLProjection, bool, error) {
	var envelope struct {
		State struct {
			Phase string `json:"phase"`
		} `json:"state"`
		NextAction struct {
			Kind string `json:"kind"`
		} `json:"next_action"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.NextAction.Kind == "" {
		return FDLProjection{}, false, fmt.Errorf("decode FDL recovery reply")
	}
	if !strings.HasPrefix(envelope.NextAction.Kind, "dispatch_") {
		return FDLProjection{}, false, nil
	}
	return FDLProjection{
		Status: "running", Phase: fdlProjectionPhase(envelope.State.Phase),
		Summary: "Controller dispatched FDL work after recovery", ActionType: envelope.NextAction.Kind,
	}, true, nil
}

func fdlDecisionAllowed(choices []string, decision string) bool {
	for _, choice := range choices {
		if decision == choice {
			return true
		}
	}
	return false
}

func (d *Daemon) submitFDLHumanDecision(ctx context.Context, runtimeID, runID string, mailbox *fdlExecutorMailbox, decision FDLHumanDecision) error {
	receiptPath := d.decisionReceiptPath(runID)
	var receipt fdlDecisionReceipt
	found, err := readFDLExecutorJSON(receiptPath, &receipt)
	if err != nil {
		return err
	}
	if found {
		if receipt.SchemaVersion != 1 || receipt.DecisionID != decision.ID || receipt.FDLRunID != mailbox.FDLRunID || receipt.ActionID != mailbox.ActionID || receipt.Decision != decision.Decision || receipt.OperationID == "" {
			return fmt.Errorf("FDL decision receipt conflicts with mailbox")
		}
	} else {
		operationID := ""
		if decision.Status == "pending" {
			operationID, err = newFDLOperationID()
			if err != nil {
				return err
			}
			decision, err = d.client.ClaimFDLHumanDecision(ctx, runtimeID, runID, decision.ID, operationID)
			if err != nil {
				return fmt.Errorf("claim FDL human decision: %w", err)
			}
		}
		if decision.Status != "processing" || decision.OperationID == nil || *decision.OperationID == "" {
			return fmt.Errorf("FDL human decision has no recoverable operation")
		}
		receipt = fdlDecisionReceipt{
			SchemaVersion: 1, DecisionID: decision.ID, FDLRunID: mailbox.FDLRunID, ActionID: mailbox.ActionID,
			Decision: decision.Decision, OperationID: *decision.OperationID, UpdatedAt: time.Now().UTC(),
		}
		if err := writeFDLExecutorJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	if !receipt.ControllerSubmitted {
		eventPath := filepath.Join(d.fdlExecutorStateDir(runID), "decision-event.json")
		event := map[string]any{
			"envelope_version": 1, "kind": "human_decision_submitted", "run_id": receipt.FDLRunID,
			"action_id": receipt.ActionID, "decision": map[string]string{"decision": receipt.Decision},
		}
		if err := writeFDLExecutorJSON(eventPath, event); err != nil {
			return err
		}
		output, err := d.runFDLCommandOutput(ctx, "drive-run", "--run-root", filepath.Join(d.cfg.FDLRunRoot, runID), "--operation-id", receipt.OperationID, "--event", eventPath)
		if err != nil {
			return fmt.Errorf("submit FDL human decision: %w", err)
		}
		if err := validateFDLReturnedEnvelope(output, receipt.FDLRunID); err != nil {
			return err
		}
		receipt.ControllerSubmitted = true
		receipt.ReturnedEnvelope = output
		receipt.UpdatedAt = time.Now().UTC()
		if err := writeFDLExecutorJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	return d.finalizeFDLDecisionReceipt(ctx, runtimeID, runID, receipt)
}

func (d *Daemon) recoverFDLDecisionReceipt(ctx context.Context, runID, runtimeID string, mailbox *fdlExecutorMailbox) error {
	var receipt fdlDecisionReceipt
	found, err := readFDLExecutorJSON(d.decisionReceiptPath(runID), &receipt)
	if err != nil || !found {
		return err
	}
	if receipt.SchemaVersion != 1 || receipt.DecisionID == "" || receipt.FDLRunID == "" || receipt.ActionID == "" || receipt.Decision == "" || receipt.OperationID == "" {
		return fmt.Errorf("invalid FDL decision receipt")
	}
	if !receipt.ControllerSubmitted {
		if mailbox == nil || mailbox.FDLRunID != receipt.FDLRunID || mailbox.ActionID != receipt.ActionID || mailbox.ActionKind != "request_human_decision" {
			return fmt.Errorf("unsubmitted FDL decision receipt conflicts with mailbox")
		}
		return nil
	}
	if err := validateFDLReturnedEnvelope(receipt.ReturnedEnvelope, receipt.FDLRunID); err != nil {
		return err
	}
	returned := fdlMailboxFromEnvelope(receipt.ReturnedEnvelope)
	if mailbox != nil && mailbox.ActionID != receipt.ActionID && mailbox.ActionID != returned.ActionID {
		return fmt.Errorf("submitted FDL decision receipt conflicts with mailbox")
	}
	return d.finalizeFDLDecisionReceipt(ctx, runtimeID, runID, receipt)
}

func (d *Daemon) finalizeFDLDecisionReceipt(ctx context.Context, runtimeID, runID string, receipt fdlDecisionReceipt) error {
	if err := d.client.CompleteFDLHumanDecision(ctx, runtimeID, runID, receipt.DecisionID, receipt.OperationID); err != nil {
		return fmt.Errorf("complete FDL human decision: %w", err)
	}
	// A planning or approval decision can immediately yield a dispatch. The
	// direct-task endpoint accepts only active runs, so restore this projection
	// before the next executor cycle materializes the returned work item.
	if projection, dispatch, err := fdlRunningProjectionForDispatch(receipt.ReturnedEnvelope); err != nil {
		return err
	} else if dispatch {
		if err := d.client.UpdateFDLIssueRunProjection(ctx, runtimeID, runID, projection); err != nil {
			return fmt.Errorf("restore FDL projection after human decision: %w", err)
		}
	}
	if err := writeFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "executor.mailbox"), fdlMailboxFromEnvelope(receipt.ReturnedEnvelope)); err != nil {
		return err
	}
	if err := os.Remove(d.decisionReceiptPath(runID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove completed FDL decision receipt: %w", err)
	}
	if err := os.Remove(filepath.Join(d.fdlExecutorStateDir(runID), "decision-event.json")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove submitted FDL decision event: %w", err)
	}
	return nil
}

func (d *Daemon) processFDLHandoff(ctx context.Context, run PendingFDLIssueRun, mailbox *fdlExecutorMailbox) error {
	if run.FDLRunID == nil || *run.FDLRunID != mailbox.FDLRunID {
		return fmt.Errorf("FDL handoff mailbox has invalid run binding")
	}
	receiptPath := filepath.Join(d.fdlExecutorStateDir(run.ID), "handoff-receipt.json")
	var receipt fdlHandoffReceipt
	found, err := readFDLExecutorJSON(receiptPath, &receipt)
	if err != nil {
		return err
	}
	if found {
		if receipt.SchemaVersion != 1 || !receipt.Verified || receipt.FDLRunID != mailbox.FDLRunID || receipt.ActionID != mailbox.ActionID || len(receipt.Handoff) == 0 {
			return fmt.Errorf("FDL handoff receipt conflicts with mailbox")
		}
	} else {
		runRoot := filepath.Join(d.cfg.FDLRunRoot, run.ID)
		verified, err := d.runFDLCommandOutput(ctx, "verify-run", "--run-root", runRoot)
		if err != nil {
			return fmt.Errorf("verify FDL delivery handoff: %w", err)
		}
		var verification struct {
			Verified bool   `json:"verified"`
			RunID    string `json:"run_id"`
			Status   string `json:"status"`
		}
		if json.Unmarshal(verified, &verification) != nil || !verification.Verified || verification.RunID != mailbox.FDLRunID || verification.Status != "completed" {
			return fmt.Errorf("FDL delivery handoff verification is invalid")
		}
		handoff, err := d.runFDLCommandOutput(ctx, "render-delivery-handoff", "--run-root", runRoot)
		if err != nil {
			return fmt.Errorf("render FDL delivery handoff: %w", err)
		}
		var document struct {
			Contract    string `json:"contract"`
			RunID       string `json:"run_id"`
			HandoffHash string `json:"handoff_hash"`
		}
		if json.Unmarshal(handoff, &document) != nil || document.Contract != "verified-change-set/v1" || document.RunID != mailbox.FDLRunID || document.HandoffHash == "" {
			return fmt.Errorf("FDL delivery handoff document is invalid")
		}
		receipt = fdlHandoffReceipt{SchemaVersion: 1, FDLRunID: mailbox.FDLRunID, ActionID: mailbox.ActionID, Verified: true, Handoff: handoff, UpdatedAt: time.Now().UTC()}
		if err := writeFDLExecutorJSON(receiptPath, receipt); err != nil {
			return err
		}
	}
	return d.client.UpdateFDLIssueRunProjection(ctx, run.FDLRuntimeID, run.ID, FDLProjection{
		Status: "handoff_ready", Phase: "handoff", Summary: "FDL run is verified and ready for an independent Git workflow.", ActionType: "handoff_external",
	})
}

func (d *Daemon) projectFDLStop(ctx context.Context, run PendingFDLIssueRun, mailbox *fdlExecutorMailbox) error {
	var envelope struct {
		RunID string `json:"run_id"`
		State struct {
			Status string `json:"status"`
			Phase  string `json:"phase"`
		} `json:"state"`
		NextAction struct {
			ActionID string `json:"action_id"`
			Kind     string `json:"kind"`
		} `json:"next_action"`
	}
	if json.Unmarshal(mailbox.Envelope, &envelope) != nil || envelope.RunID != mailbox.FDLRunID || envelope.NextAction.ActionID != mailbox.ActionID || envelope.NextAction.Kind != "stop" || (envelope.State.Status != "failed" && envelope.State.Status != "cancelled") {
		return fmt.Errorf("decode terminal FDL Controller envelope")
	}
	phase := "complete"
	if envelope.State.Status == "cancelled" {
		phase = "cancelled"
	}
	return d.client.UpdateFDLIssueRunProjection(ctx, run.FDLRuntimeID, run.ID, FDLProjection{
		Status: envelope.State.Status, Phase: phase, Summary: "FDL Controller stopped the delivery run.", ActionType: "stop",
	})
}

// collectFDLTaskResults is intentionally independent from a mailbox action:
// after acknowledgement the Controller normally says await_external_results,
// while the private bindings retain the original action/token identity needed
// to submit each terminal result exactly once.
func (d *Daemon) collectFDLTaskResults(ctx context.Context, runID string, controllerRunID string) error {
	itemsDir := filepath.Join(d.fdlExecutorStateDir(runID), "work-items")
	entries, err := os.ReadDir(itemsDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list FDL work items: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Superseded retry bindings are retained under archive/ for audit, but
		// no longer represent live Controller work items.
		if entry.Name() == "archive" {
			continue
		}
		var binding fdlWorkItemBinding
		found, err := readFDLExecutorJSON(d.fdlBindingPath(runID, entry.Name()), &binding)
		if err != nil {
			return fmt.Errorf("read FDL work-item binding: %w", err)
		}
		if !found {
			return fmt.Errorf("FDL work-item binding is missing")
		}
		if binding.FDLRunID != controllerRunID || binding.TaskID == "" || binding.TerminalSubmitted || binding.State == "dispatch_failed" {
			continue
		}
		status, err := d.client.GetTaskStatus(ctx, binding.TaskID)
		if err != nil {
			return fmt.Errorf("read FDL task %s status: %w", binding.WorkItemID, err)
		}
		if status != "completed" && status != "failed" && status != "cancelled" {
			continue
		}
		if err := d.submitFDLTerminalTask(ctx, runID, &binding, status); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) submitFDLTerminalTask(ctx context.Context, runID string, binding *fdlWorkItemBinding, taskStatus string) error {
	itemDir := filepath.Join(d.fdlExecutorStateDir(runID), "work-items", binding.WorkItemID)
	eventPath := filepath.Join(itemDir, "terminal-event.json")
	if binding.TerminalOperationID == "" {
		event, err := d.buildFDLTerminalEvent(ctx, runID, *binding, taskStatus)
		if err != nil {
			event = fdlTaskFailureEvent(*binding, "invalid_result", boundedFDLError(err))
		}
		if err := writeFDLExecutorJSON(eventPath, event); err != nil {
			return err
		}
		operationID, err := newFDLOperationID()
		if err != nil {
			return err
		}
		binding.TerminalOperationID = operationID
		binding.State = "terminal_submitting"
		binding.UpdatedAt = time.Now().UTC()
		if err := writeFDLExecutorJSON(d.fdlBindingPath(runID, binding.WorkItemID), binding); err != nil {
			return err
		}
	}
	output, err := d.runFDLCommandOutput(ctx, "drive-run", "--run-root", filepath.Join(d.cfg.FDLRunRoot, runID), "--operation-id", binding.TerminalOperationID, "--event", eventPath)
	if err != nil {
		if fdlControllerRejectedTerminalResult(err) {
			failure := fdlTaskFailureEvent(*binding, "invalid_result", boundedFDLError(err))
			if writeErr := writeFDLExecutorJSON(eventPath, failure); writeErr != nil {
				return writeErr
			}
			operationID, operationErr := newFDLOperationID()
			if operationErr != nil {
				return operationErr
			}
			binding.TerminalOperationID = operationID
			binding.State = "terminal_submitting"
			binding.UpdatedAt = time.Now().UTC()
			return writeFDLExecutorJSON(d.fdlBindingPath(runID, binding.WorkItemID), binding)
		}
		return fmt.Errorf("submit FDL terminal result for %s: %w", binding.WorkItemID, err)
	}
	if err := validateFDLReturnedEnvelope(output, binding.FDLRunID); err != nil {
		return err
	}
	if err := writeFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "executor.mailbox"), fdlMailboxFromEnvelope(output)); err != nil {
		return err
	}
	binding.TerminalSubmitted = true
	binding.State = "terminal_submitted"
	binding.UpdatedAt = time.Now().UTC()
	return writeFDLExecutorJSON(d.fdlBindingPath(runID, binding.WorkItemID), binding)
}

func fdlControllerRejectedTerminalResult(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FDL drive-run: exit status 2:")
}

func fdlTaskFailureEvent(binding fdlWorkItemBinding, kind, reason string) map[string]any {
	return map[string]any{
		"envelope_version": 1, "kind": "work_item_failed", "run_id": binding.FDLRunID,
		"external_work_id": binding.ExternalWorkID, "work_item_id": binding.WorkItemID,
		"submission_token": binding.SubmissionToken, "origin_action_id": binding.ActionID,
		"failure": map[string]string{"kind": kind, "reason": reason},
	}
}

func (d *Daemon) buildFDLTerminalEvent(ctx context.Context, runID string, binding fdlWorkItemBinding, taskStatus string) (map[string]any, error) {
	if taskStatus != "completed" {
		return fdlTaskFailureEvent(binding, "multica_task_"+taskStatus, "direct Multica Agent task ended as "+taskStatus), nil
	}
	itemDir := filepath.Join(d.fdlExecutorStateDir(runID), "work-items", binding.WorkItemID)
	if binding.Role == "explorer:impact_analysis" {
		advisory, err := readFDLExplorerAdvisory(filepath.Join(itemDir, "advisory.json"), binding.WorkItemID)
		if err != nil {
			return nil, err
		}
		encoded, err := canonicalFDLJSON(advisory)
		if err != nil {
			return nil, err
		}
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
		return map[string]any{
			"envelope_version": 1, "kind": "work_item_completed", "run_id": binding.FDLRunID,
			"external_work_id": binding.ExternalWorkID, "work_item_id": binding.WorkItemID,
			"submission_token": binding.SubmissionToken, "origin_action_id": binding.ActionID,
			"result": map[string]any{"media_type": "application/json", "sha256": digest, "content": advisory},
		}, nil
	}
	return d.buildFDLRoleCompletionEvent(ctx, runID, binding, itemDir)
}

func readFDLExplorerAdvisory(path, workItemID string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Explorer advisory: %w", err)
	}
	var advisory map[string]any
	if json.Unmarshal(data, &advisory) != nil || advisory["schema_version"] != float64(1) || advisory["request_id"] != workItemID {
		return nil, fmt.Errorf("Explorer advisory is invalid")
	}
	return advisory, nil
}

func canonicalFDLJSON(value any) ([]byte, error) {
	var encoded bytes.Buffer
	if err := appendCanonicalFDLJSON(&encoded, value); err != nil {
		return nil, err
	}
	// FDL's digest contract includes exactly one final newline.
	encoded.WriteByte('\n')
	return encoded.Bytes(), nil
}

const fdlMaxSafeJSONInteger = 9007199254740991

// appendCanonicalFDLJSON mirrors FDL's cross-language canonical JSON: object
// keys are ordered by UTF-16BE bytes, strings preserve non-ASCII data, and
// only interoperable integers are accepted. Explorer output is untrusted, so
// rejecting values outside this narrow contract is preferable to a digest the
// Python Controller cannot reproduce.
func appendCanonicalFDLJSON(out *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, err := marshalFDLJSONString(v)
		if err != nil {
			return err
		}
		out.Write(encoded)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < -fdlMaxSafeJSONInteger || v > fdlMaxSafeJSONInteger {
			return fmt.Errorf("FDL canonical JSON only permits interoperable integers")
		}
		out.WriteString(strconv.FormatInt(int64(v), 10))
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalFDLJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			return bytes.Compare(fdlUTF16BE(keys[i]), fdlUTF16BE(keys[j])) < 0
		})
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, err := marshalFDLJSONString(key)
			if err != nil {
				return err
			}
			out.Write(encoded)
			out.WriteByte(':')
			if err := appendCanonicalFDLJSON(out, v[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("FDL canonical JSON rejects %T", value)
	}
	return nil
}

func marshalFDLJSONString(value string) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), nil
}

func fdlUTF16BE(value string) []byte {
	codeUnits := utf16.Encode([]rune(value))
	encoded := make([]byte, len(codeUnits)*2)
	for i, unit := range codeUnits {
		encoded[i*2] = byte(unit >> 8)
		encoded[i*2+1] = byte(unit)
	}
	return encoded
}

func (d *Daemon) buildFDLRoleCompletionEvent(ctx context.Context, runID string, binding fdlWorkItemBinding, itemDir string) (map[string]any, error) {
	reportPath := filepath.Join(itemDir, "report.md")
	report, err := os.ReadFile(reportPath)
	if err != nil || len(bytes.TrimSpace(report)) == 0 {
		return nil, fmt.Errorf("FDL role report is missing or empty")
	}
	meta, err := readFDLRoleMeta(filepath.Join(itemDir, "meta.json"))
	if err != nil {
		return nil, err
	}
	payload, err := d.readFDLDispatchPayload(runID, binding.WorkItemID)
	if err != nil {
		return nil, err
	}
	args := []string{"complete-work-item", "--run-root", filepath.Join(d.cfg.FDLRunRoot, runID), "--work-item-id", binding.WorkItemID, "--report", reportPath}
	if payload.Attempt.Phase == "planning" || payload.Attempt.Phase == "design" {
		contract, ok := meta["contract_index"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("FDL %s role metadata requires a contract_index object", payload.Attempt.Phase)
		}
		pack, err := d.fdlContextPack(runID, binding, payload, string(report), contract)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(itemDir, "submission-context_pack.json")
		if err := writeFDLExecutorJSON(path, pack); err != nil {
			return nil, err
		}
		args = append(args, "--context-pack", path)
		// This artifact binds private Controller evidence. Never accept an
		// Agent-authored substitute for it.
		delete(meta, "context_pack")
	}
	for _, field := range []string{"contract_index", "context_pack", "findings", "finding_resolutions", "blockers", "failure"} {
		if value, ok := meta[field]; ok {
			path := filepath.Join(itemDir, "submission-"+field+".json")
			if err := writeFDLExecutorJSON(path, value); err != nil {
				return nil, err
			}
			args = append(args, "--"+strings.ReplaceAll(field, "_", "-"), path)
		}
	}
	if outcome, ok := meta["outcome"].(string); ok {
		if outcome != "completed" && outcome != "blocked" && outcome != "failed" {
			return nil, fmt.Errorf("FDL role metadata outcome is invalid")
		}
		args = append(args, "--outcome", outcome)
	}
	if decision, ok := meta["decision"].(string); ok {
		if decision != "accepted" && decision != "changes_requested" {
			return nil, fmt.Errorf("FDL role metadata decision is invalid")
		}
		args = append(args, "--decision", decision)
	}
	if consistency, ok := meta["evidence_consistency"].(string); ok {
		if consistency != "checked" && consistency != "not_applicable" && consistency != "conflict_found" {
			return nil, fmt.Errorf("FDL role metadata evidence consistency is invalid")
		}
		args = append(args, "--evidence-consistency", consistency)
	} else if fdlRequiresEvidenceConsistency(payload) {
		return nil, fmt.Errorf("FDL role metadata requires evidence_consistency for Context Pack evidence")
	}
	if fdlExtractsWorkspaceChanges(payload) {
		args = append(args, "--changed-paths-from-workspace")
	}
	output, err := d.runFDLCommandOutput(ctx, args...)
	if err != nil {
		return nil, err
	}
	var event map[string]any
	if err := json.Unmarshal(output, &event); err != nil {
		return nil, fmt.Errorf("decode complete-work-item result: %w", err)
	}
	return event, nil
}

func (d *Daemon) fdlContextPack(runID string, binding fdlWorkItemBinding, payload fdlDispatchPayload, report string, contract map[string]any) (map[string]any, error) {
	if payload.Attempt.AttemptID == "" || payload.Attempt.ControlManifestHash == "" {
		return nil, fmt.Errorf("FDL planning dispatch has incomplete private identity")
	}
	taskBriefHash, err := fdlTaskBriefHash(filepath.Join(d.cfg.FDLRunRoot, runID, "state.json"))
	if err != nil {
		return nil, err
	}
	artifactKind := "design"
	if payload.Attempt.Phase == "planning" {
		artifactKind = "delivery_plan"
	}
	artifactHash, err := fdlValueDigest(map[string]any{"kind": artifactKind, "media_type": "text/markdown", "content": report})
	if err != nil {
		return nil, err
	}
	contractHash, err := fdlValueDigest(contract)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"schema_version": 1,
		"artifact_kind":  "context_pack",
		"producer":       map[string]any{"phase": payload.Attempt.Phase, "attempt_id": payload.Attempt.AttemptID, "dispatch_hash": binding.DispatchSHA256},
		"bindings": map[string]any{
			"task_brief_hash":       taskBriefHash,
			"design_or_plan_hash":   artifactHash,
			"contract_index_hash":   contractHash,
			"control_manifest_hash": payload.Attempt.ControlManifestHash,
		},
		"code_map":             []any{},
		"invariants":           []any{},
		"decisions":            []any{},
		"rejected_options":     []any{},
		"open_risks":           []any{},
		"acceptance_test_map":  []any{},
		"exploration_evidence": []any{},
	}, nil
}

func fdlTaskBriefHash(statePath string) (any, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("read FDL Controller state for Context Pack: %w", err)
	}
	var state struct {
		Roots map[string]struct {
			Artifact struct {
				SHA256 string `json:"sha256"`
			} `json:"artifact"`
		} `json:"roots"`
	}
	if json.Unmarshal(data, &state) != nil {
		return nil, fmt.Errorf("decode FDL Controller state for Context Pack")
	}
	if taskBrief, ok := state.Roots["task_brief"]; ok && taskBrief.Artifact.SHA256 != "" {
		return taskBrief.Artifact.SHA256, nil
	}
	return nil, nil
}

func fdlValueDigest(value any) (string, error) {
	encoded, err := canonicalFDLJSON(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}

func readFDLRoleMeta(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read FDL role metadata: %w", err)
	}
	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return nil, fmt.Errorf("FDL role metadata is invalid JSON")
	}
	for key := range meta {
		switch key {
		case "outcome", "decision", "evidence_consistency", "contract_index", "context_pack", "changed_paths_from_workspace", "findings", "finding_resolutions", "blockers", "failure":
		default:
			return nil, fmt.Errorf("FDL role metadata contains unsupported field %q", key)
		}
	}
	return meta, nil
}

// materializeFDLDispatch creates per-item private input/result directories and
// records a durable pre-dispatch binding. A crash after that point fails closed
// rather than reposting an ambiguous direct task. The subsequent acknowledgement
// path is intentionally responsible for advancing the Controller.
func (d *Daemon) materializeFDLDispatch(runID string, mailbox *fdlExecutorMailbox) ([]fdlWorkItemBinding, error) {
	var envelope fdlDispatchEnvelope
	if err := json.Unmarshal(mailbox.Envelope, &envelope); err != nil {
		return nil, fmt.Errorf("decode FDL dispatch mailbox: %w", err)
	}
	if envelope.RunID != mailbox.FDLRunID || envelope.NextAction.ActionID != mailbox.ActionID || !strings.HasPrefix(envelope.NextAction.Kind, "dispatch_") || envelope.NextAction.ExternalWorkID == "" || len(envelope.NextAction.WorkItems) == 0 {
		return nil, fmt.Errorf("mailbox does not contain a dispatch action")
	}
	runRoot := filepath.Join(d.cfg.FDLRunRoot, runID)
	stateDir := d.fdlExecutorStateDir(runID)
	bindings := make([]fdlWorkItemBinding, 0, len(envelope.NextAction.WorkItems))
	for _, item := range envelope.NextAction.WorkItems {
		if item.WorkItemID == "" || item.SubmissionToken == "" || item.Dispatch.Path == "" || item.Dispatch.SHA256 == "" {
			return nil, fmt.Errorf("invalid FDL dispatch work item")
		}
		dispatchPath, err := fdlRunFilePath(runRoot, item.Dispatch.Path)
		if err != nil {
			return nil, err
		}
		dispatch, err := os.ReadFile(dispatchPath)
		if err != nil || "sha256:"+fmt.Sprintf("%x", sha256.Sum256(dispatch)) != item.Dispatch.SHA256 {
			return nil, fmt.Errorf("dispatch evidence hash mismatch for %s", item.WorkItemID)
		}
		var payload fdlDispatchPayload
		if json.Unmarshal(dispatch, &payload) != nil || payload.Attempt.Role == "" {
			return nil, fmt.Errorf("invalid dispatch evidence for %s", item.WorkItemID)
		}
		itemDir := filepath.Join(stateDir, "work-items", item.WorkItemID)
		if err := os.MkdirAll(itemDir, 0o700); err != nil {
			return nil, err
		}
		bindingPath := filepath.Join(itemDir, "binding.json")
		var existing fdlWorkItemBinding
		if found, err := readFDLExecutorJSON(bindingPath, &existing); err != nil {
			return nil, err
		} else if found {
			if existing.FDLRunID == envelope.RunID && existing.ActionID == envelope.NextAction.ActionID && existing.WorkItemID == item.WorkItemID && existing.SubmissionToken == item.SubmissionToken {
				bindings = append(bindings, existing)
				continue
			}
			// A recovery retry gets a new Controller action and submission token,
			// while its logical work_item_id may remain the same. Preserve the
			// terminal binding under an action-scoped archive before creating the
			// replacement; reusing the path would bind the new token to old work.
			if !fdlBindingCanBeArchived(existing) {
				return nil, fmt.Errorf("existing FDL work-item binding conflicts with current action")
			}
			archiveID := fmt.Sprintf("%x", sha256.Sum256([]byte(existing.ActionID)))
			archiveDir := filepath.Join(stateDir, "work-items", "archive", archiveID, item.WorkItemID)
			if err := os.MkdirAll(filepath.Dir(archiveDir), 0o700); err != nil {
				return nil, fmt.Errorf("create archived FDL work-item directory: %w", err)
			}
			if _, err := os.Stat(archiveDir); err == nil {
				return nil, fmt.Errorf("archived FDL work-item binding already exists")
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("inspect archived FDL work-item binding: %w", err)
			}
			if err := os.Rename(itemDir, archiveDir); err != nil {
				return nil, fmt.Errorf("archive superseded FDL work-item binding: %w", err)
			}
			if err := os.MkdirAll(itemDir, 0o700); err != nil {
				return nil, fmt.Errorf("create replacement FDL work-item directory: %w", err)
			}
		}
		if err := os.WriteFile(filepath.Join(itemDir, "dispatch.json"), dispatch, 0o400); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(itemDir, "report.md"), nil, 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(itemDir, "meta.json"), []byte("{}\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(itemDir, "advisory.json"), []byte("{}\n"), 0o600); err != nil {
			return nil, err
		}
		dispatchKey, err := newFDLOperationID()
		if err != nil {
			return nil, err
		}
		binding := fdlWorkItemBinding{SchemaVersion: 1, FDLRunID: envelope.RunID, ActionID: envelope.NextAction.ActionID, ExternalWorkID: envelope.NextAction.ExternalWorkID, WorkItemID: item.WorkItemID, SubmissionToken: item.SubmissionToken, DispatchSHA256: item.Dispatch.SHA256, DispatchKey: dispatchKey, Role: payload.Attempt.Role, State: "prepared", UpdatedAt: time.Now().UTC()}
		if err := writeFDLExecutorJSON(bindingPath, binding); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].WorkItemID < bindings[j].WorkItemID })
	return bindings, nil
}

// fdlBindingCanBeArchived admits only work the Controller has already
// terminally consumed. A dispatch_failed binding reaches that state through
// the batch acknowledgement, even though it never has an Agent result.
func fdlBindingCanBeArchived(binding fdlWorkItemBinding) bool {
	return binding.TerminalSubmitted || binding.State == "dispatch_failed"
}

func fdlRunFilePath(runRoot, referencePath string) (string, error) {
	if filepath.IsAbs(referencePath) {
		return "", fmt.Errorf("FDL dispatch reference must be relative")
	}
	path := filepath.Clean(filepath.Join(runRoot, referencePath))
	rel, err := filepath.Rel(runRoot, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("FDL dispatch reference escapes run root")
	}
	return path, nil
}

func boundedFDLError(err error) string { return boundedFDLErrorText(err.Error()) }

func boundedFDLErrorText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 300 {
		return value[:300]
	}
	return value
}
