package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIsFDLExternalRootRejectsNestedPaths(t *testing.T) {
	tests := []struct {
		runRoot  string
		repoRoot string
		want     bool
	}{
		{runRoot: "/var/fdl-runs", repoRoot: "/Users/example/repo", want: true},
		{runRoot: "/Users/example/repo/.fdl-runs", repoRoot: "/Users/example/repo", want: false},
		{runRoot: "/Users/example", repoRoot: "/Users/example/repo", want: false},
		{runRoot: "/Users/example/repo", repoRoot: "/Users/example/repo", want: false},
	}
	for _, tt := range tests {
		if got := isFDLExternalRoot(tt.runRoot, tt.repoRoot); got != tt.want {
			t.Errorf("isFDLExternalRoot(%q, %q) = %v, want %v", tt.runRoot, tt.repoRoot, got, tt.want)
		}
	}
}

func TestFDLProfileRoleMapsControllerRolesWithoutFallback(t *testing.T) {
	tests := []struct {
		name         string
		controller   string
		instructions string
		want         string
		wantErr      bool
	}{
		{name: "intake", controller: "feature-delivery-intake", instructions: `{}`, want: "intake"},
		{name: "planner", controller: "feature-delivery-planner", instructions: `{}`, want: "planner"},
		{name: "implementer", controller: "feature-delivery-implementer", instructions: `{}`, want: "implementer"},
		{name: "review regression", controller: "feature-delivery-reviewer", instructions: `{"lane":"regression"}`, want: "reviewer:regression"},
		{name: "conflict recheck", controller: "feature-delivery-reviewer", instructions: `{"lane":"conflict_recheck"}`, want: "reviewer:correctness"},
		{name: "unknown role", controller: "untrusted-role", instructions: `{}`, wantErr: true},
		{name: "unknown review lane", controller: "feature-delivery-reviewer", instructions: `{"lane":"new_lane"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := fdlDispatchPayload{}
			payload.Attempt.Role = tt.controller
			payload.Instructions = json.RawMessage(tt.instructions)
			got, err := fdlProfileRole(payload)
			if tt.wantErr {
				if err == nil {
					t.Fatal("fdlProfileRole unexpectedly succeeded")
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("fdlProfileRole() = (%q, %v), want (%q, nil)", got, err, tt.want)
			}
		})
	}
}

func TestFDLDispatchAcknowledgementCoversTasksAndFailures(t *testing.T) {
	d := &Daemon{cfg: Config{DaemonID: "daemon-test"}}
	receipt := &fdlDispatchReceipt{
		SchemaVersion: 1, FDLRunID: "fdl-run", ActionID: "action", ExternalWorkID: "work",
		TaskIDs:          map[string]string{"implement": "task-1", "review": "task-2"},
		DispatchFailures: map[string]string{"preflight": "unsupported local operator"}, Activated: map[string]bool{},
	}
	event := d.fdlAcknowledgementEvent(receipt)
	if event["kind"] != "external_work_acknowledged" || event["run_id"] != "fdl-run" || event["action_id"] != "action" {
		t.Fatalf("ack event lost controller binding: %#v", event)
	}
	acks, ok := event["acknowledgements"].([]map[string]string)
	if !ok || len(acks) != 3 {
		t.Fatalf("acknowledgements = %#v, want 3 entries", event["acknowledgements"])
	}
	byID := map[string]map[string]string{}
	for _, ack := range acks {
		byID[ack["work_item_id"]] = ack
		if ack["executor_identity"] != "multica-daemon:daemon-test" || ack["identity_assurance"] != "human_attested" {
			t.Fatalf("unsafe acknowledgement identity: %#v", ack)
		}
	}
	if byID["preflight"]["status"] != "dispatch_failed" || byID["implement"]["status"] != "acknowledged" || byID["review"]["status"] != "acknowledged" {
		t.Fatalf("acknowledgement statuses = %#v", byID)
	}
}

func TestFDLDispatchFaultIsOptIn(t *testing.T) {
	d := &Daemon{}
	if err := d.fdlDispatchFault("after_acknowledgement"); err != nil {
		t.Fatalf("nil fault hook returned %v", err)
	}
	want := errors.New("simulated daemon crash")
	d.fdlDispatchCheckpoint = func(point string) error {
		if point != "after_activation" {
			t.Fatalf("fault point = %q", point)
		}
		return want
	}
	if err := d.fdlDispatchFault("after_activation"); !errors.Is(err, want) {
		t.Fatalf("fault hook error = %v, want %v", err, want)
	}
}

func TestNewFDLDispatchCheckpointIsDisabledInProductionBuild(t *testing.T) {
	if checkpoint := newFDLDispatchCheckpoint(); checkpoint != nil {
		t.Fatal("production build unexpectedly enabled an FDL crash checkpoint")
	}
}

func TestFDLPlanningExplorationEventPreservesPrivateBinding(t *testing.T) {
	binding := fdlWorkItemBinding{FDLRunID: "fdl4-run", ExternalWorkID: "work-plan", WorkItemID: "planning", SubmissionToken: "token-private"}
	event := fdlPlanningExplorationEvent(binding, []any{map[string]any{
		"request_id": "impact-analysis", "lane": "impact_analysis", "question": "Inspect one file.",
		"allowed_paths": []any{"src/sample.py"}, "expected_evidence": []any{"files"},
	}})
	if event["kind"] != "planning_exploration_requested" || event["run_id"] != binding.FDLRunID || event["external_work_id"] != binding.ExternalWorkID || event["work_item_id"] != binding.WorkItemID || event["submission_token"] != binding.SubmissionToken {
		t.Fatalf("exploration event lost Controller binding: %#v", event)
	}
}

func TestReadFDLRoleMetaRejectsUnknownFields(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"private token":       {"outcome": "completed", "token": "must-not-pass"},
		"contract header":     {"schema_version": 1, "artifact_kind": "task_brief"},
		"contract field typo": {"contract": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meta.json")
			if err := writeFDLExecutorJSON(path, meta); err != nil {
				t.Fatalf("write metadata: %v", err)
			}
			if _, err := readFDLRoleMeta(path); err == nil {
				t.Fatal("metadata with an unknown field was accepted")
			}
		})
	}
}

func TestReadFDLRoleMetaAllowsReviewerFindingResolutions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := writeFDLExecutorJSON(path, map[string]any{"finding_resolutions": []any{}}); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if _, err := readFDLRoleMeta(path); err != nil {
		t.Fatalf("reviewer finding resolutions were rejected: %v", err)
	}
}

func TestFDLAgentInstructionsRequireConsistencyForContextPackConsumers(t *testing.T) {
	d := &Daemon{cfg: Config{FDLRunRoot: t.TempDir()}}
	if err := d.persistFDLIssueInput("run", json.RawMessage(`{"title":"Set the expected value","description":"Implement the frozen requirement.","priority":"high","project_id":"not-projected"}`)); err != nil {
		t.Fatalf("persist issue input: %v", err)
	}
	payload := fdlDispatchPayload{Instructions: json.RawMessage(`{"task":"implement"}`), InputHashes: map[string]string{"context_pack_hash": "sha256:pack"}}
	payload.Attempt.Role = "feature-delivery-implementer"
	payload.Attempt.Phase = "implementation"
	instructions, err := d.fdlAgentInstructions("run", fdlWorkItemBinding{WorkItemID: "implementation", Role: "implementer"}, payload)
	if err != nil {
		t.Fatalf("build instructions: %v", err)
	}
	if !strings.Contains(instructions, "MUST write meta.json") || !strings.Contains(instructions, "never use not_applicable") {
		t.Fatalf("Context Pack consistency requirement missing from instructions: %s", instructions)
	}
	if !strings.Contains(instructions, "Set the expected value") || strings.Contains(instructions, "not-projected") {
		t.Fatalf("frozen Issue requirements seed was not safely projected: %s", instructions)
	}
}

func TestFDLAgentInstructionsReserveContextPackForPlanning(t *testing.T) {
	d := &Daemon{cfg: Config{FDLRunRoot: t.TempDir()}}
	if err := d.persistFDLIssueInput("run", json.RawMessage(`{"title":"Review","description":"Review the frozen snapshot.","priority":"normal"}`)); err != nil {
		t.Fatal(err)
	}
	payload := fdlDispatchPayload{Instructions: json.RawMessage(`{"lane":"correctness"}`)}
	payload.Attempt.Role = "feature-delivery-reviewer"
	payload.Attempt.Phase = "review"
	instructions, err := d.fdlAgentInstructions("run", fdlWorkItemBinding{WorkItemID: "correctness", Role: "reviewer:correctness"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions, "Only an intake, design, or compact planning phase may set contract_index") {
		t.Fatalf("review instructions did not reserve planning evidence: %s", instructions)
	}
	if !strings.Contains(instructions, "Do not execute any Gate command") || !strings.Contains(instructions, "Every finding must have exactly") {
		t.Fatalf("review instructions did not provide the strict result contract: %s", instructions)
	}
}

func TestFDLAgentInstructionsUseControllerPhaseContract(t *testing.T) {
	d := &Daemon{cfg: Config{FDLRunRoot: t.TempDir()}}
	if err := d.persistFDLIssueInput("run", json.RawMessage(`{"title":"Delivery","description":"Frozen requirement.","priority":"normal"}`)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		phase       string
		contains    []string
		notContains []string
	}{
		{
			name: "intake task brief", phase: "intake",
			contains:    []string{`"contract_index":{"schema_version":1,"artifact_kind":"task_brief"`, "never at the top level of meta.json", "acceptance criteria only", "Do not request Explorer work during intake"},
			notContains: []string{`"artifact_kind":"delivery_plan"`, "exploration_requests only"},
		},
		{
			name: "design contract", phase: "design",
			contains:    []string{`"contract_index":{"schema_version":1,"artifact_kind":"design"`, "sha256:task-brief", "exploration_requests only"},
			notContains: []string{`"artifact_kind":"delivery_plan"`},
		},
		{
			name: "compact plan", phase: "planning",
			contains:    []string{`"contract_index":{"schema_version":1,"artifact_kind":"delivery_plan"`, "Do not request Explorer work on this compact planning dispatch"},
			notContains: []string{`"artifact_kind":"task_brief"`, "exploration_requests only"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := fdlDispatchPayload{Instructions: json.RawMessage(`{}`), InputHashes: map[string]string{"task_brief_hash": "sha256:task-brief"}}
			payload.Attempt.Phase = tt.phase
			instructions, err := d.fdlAgentInstructions("run", fdlWorkItemBinding{WorkItemID: tt.phase, Role: "planner"}, payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range tt.contains {
				if !strings.Contains(instructions, value) {
					t.Fatalf("instructions missing %q: %s", value, instructions)
				}
			}
			for _, value := range tt.notContains {
				if strings.Contains(instructions, value) {
					t.Fatalf("instructions unexpectedly contain %q: %s", value, instructions)
				}
			}
		})
	}
}

func TestFDLAgentInstructionsDoNotRepeatCompletedExplorerRound(t *testing.T) {
	d := &Daemon{cfg: Config{FDLRunRoot: t.TempDir()}}
	if err := d.persistFDLIssueInput("run", json.RawMessage(`{"title":"Delivery","description":"Request Explorer before Design.","priority":"normal"}`)); err != nil {
		t.Fatal(err)
	}
	payload := fdlDispatchPayload{Instructions: json.RawMessage(`{}`), InputHashes: map[string]string{
		"task_brief_hash":          "sha256:task-brief",
		"exploration_round_1_hash": "sha256:exploration",
	}}
	payload.Attempt.Phase = "design"
	instructions, err := d.fdlAgentInstructions("run", fdlWorkItemBinding{WorkItemID: "planning", Role: "planner"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions, "Explorer round has already completed") || !strings.Contains(instructions, "Do not request Explorer work again") {
		t.Fatalf("completed Explorer instruction missing: %s", instructions)
	}
	if strings.Contains(instructions, "exploration_requests only") {
		t.Fatalf("completed Explorer instruction still offers another request: %s", instructions)
	}
}

func TestPersistFDLIssueInputRejectsMissingOrOversizedTitle(t *testing.T) {
	d := &Daemon{cfg: Config{FDLRunRoot: t.TempDir()}}
	if err := d.persistFDLIssueInput("run", json.RawMessage(`{"description":"missing title"}`)); err == nil {
		t.Fatal("missing title was accepted")
	}
	tooLong := strings.Repeat("x", fdlIssueInputTitleMaxRunes+1)
	raw, err := json.Marshal(map[string]string{"title": tooLong})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.persistFDLIssueInput("run", raw); err == nil {
		t.Fatal("oversized title was accepted")
	}
}

func TestPersistFDLIssueInputDropsUnapprovedFields(t *testing.T) {
	d := &Daemon{cfg: Config{FDLRunRoot: t.TempDir()}}
	if err := d.persistFDLIssueInput("run", json.RawMessage(`{"title":"Keep this","description":"Only requirements survive.","priority":"normal","token":"must-not-persist","local_path":"/private/path"}`)); err != nil {
		t.Fatalf("persist issue input: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(d.fdlExecutorStateDir("run"), "issue-input.json"))
	if err != nil {
		t.Fatalf("read persisted issue input: %v", err)
	}
	if strings.Contains(string(data), "must-not-persist") || strings.Contains(string(data), "/private/path") {
		t.Fatalf("private snapshot fields leaked into issue input: %s", data)
	}
}

func TestEnsureFDLIssueInputBackfillsOnlyWhenMissing(t *testing.T) {
	d := &Daemon{cfg: Config{FDLRunRoot: t.TempDir()}}
	run := PendingFDLIssueRun{ID: "run", IssueSnapshot: json.RawMessage(`{"title":"Backfill requirements","description":"Frozen.","priority":"normal"}`)}
	if err := d.ensureFDLIssueInput(run); err != nil {
		t.Fatalf("backfill missing issue input: %v", err)
	}
	if _, err := d.readFDLIssueInput(run.ID); err != nil {
		t.Fatalf("read backfilled issue input: %v", err)
	}
	path := filepath.Join(d.fdlExecutorStateDir(run.ID), "issue-input.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"title":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureFDLIssueInput(run); err == nil {
		t.Fatal("malformed existing issue input was overwritten")
	}
}

func TestFDLRequiresEvidenceConsistency(t *testing.T) {
	tests := []struct {
		name   string
		phase  string
		hashes map[string]string
		want   bool
	}{
		{name: "planning producer", phase: "planning", want: true},
		{name: "design producer", phase: "design", want: true},
		{name: "implementation consumer", phase: "implementation", hashes: map[string]string{"context_pack_hash": "sha256:pack"}, want: true},
		{name: "ordinary implementation", phase: "implementation", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := fdlDispatchPayload{InputHashes: tt.hashes}
			payload.Attempt.Phase = tt.phase
			if got := fdlRequiresEvidenceConsistency(payload); got != tt.want {
				t.Fatalf("fdlRequiresEvidenceConsistency() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFDLBuildsContextPackOnlyForCompletedDesignOrCompactPlanning(t *testing.T) {
	tests := []struct {
		phase   string
		outcome string
		want    bool
	}{
		{phase: "intake", outcome: "completed", want: false},
		{phase: "design", outcome: "completed", want: true},
		{phase: "planning", outcome: "completed", want: true},
		{phase: "design", outcome: "blocked", want: false},
		{phase: "planning", outcome: "failed", want: false},
	}
	for _, tt := range tests {
		if got := fdlBuildsContextPack(tt.phase, tt.outcome); got != tt.want {
			t.Errorf("fdlBuildsContextPack(%q, %q) = %v, want %v", tt.phase, tt.outcome, got, tt.want)
		}
	}
}

func TestFDLHasExplorerEvidence(t *testing.T) {
	if fdlHasExplorerEvidence(fdlDispatchPayload{InputHashes: map[string]string{"task_brief_hash": "sha256:brief"}}) {
		t.Fatal("unrelated input hash was treated as Explorer evidence")
	}
	if !fdlHasExplorerEvidence(fdlDispatchPayload{InputHashes: map[string]string{"exploration_round_1_hash": "sha256:exploration"}}) {
		t.Fatal("completed Explorer evidence was not recognized")
	}
}

func TestFDLExtractsWorkspaceChangesOnlyForImplementation(t *testing.T) {
	for _, phase := range []string{"planning", "design", "implementation", "review"} {
		t.Run(phase, func(t *testing.T) {
			payload := fdlDispatchPayload{}
			payload.Attempt.Phase = phase
			if got, want := fdlExtractsWorkspaceChanges(payload), phase == "implementation"; got != want {
				t.Fatalf("fdlExtractsWorkspaceChanges(%q) = %v, want %v", phase, got, want)
			}
		})
	}
}

func TestFDLControllerRejectedTerminalResult(t *testing.T) {
	if !fdlControllerRejectedTerminalResult(fmt.Errorf("FDL drive-run: exit status 2: invalid result")) {
		t.Fatal("Controller validation failure was not recognized")
	}
	if fdlControllerRejectedTerminalResult(fmt.Errorf("FDL drive-run: exit status 1: temporary transport failure")) {
		t.Fatal("transport failure was incorrectly treated as a Controller validation failure")
	}
}

func TestCanonicalFDLJSONUsesProtocolOrderingAndTrailingNewline(t *testing.T) {
	encoded, err := canonicalFDLJSON(map[string]any{
		"\ue000":     float64(2),
		"\U0001f600": float64(1),
	})
	if err != nil {
		t.Fatalf("canonicalFDLJSON: %v", err)
	}
	// UTF-16BE places the surrogate-pair key before U+E000. UTF-8 sorting
	// would incorrectly reverse these two entries.
	want := "{\"😀\":1,\"\ue000\":2}\n"
	if string(encoded) != want {
		t.Fatalf("canonical FDL JSON = %q, want %q", encoded, want)
	}
	if _, err := canonicalFDLJSON(map[string]any{"fraction": 1.5}); err == nil || !strings.Contains(err.Error(), "integers") {
		t.Fatalf("fractional Explorer number was accepted: %v", err)
	}
}

func TestFDLExecutorLeaseAndMailboxRecoverAcrossHolderRestart(t *testing.T) {
	stateDir := t.TempDir()
	if err := withFDLExecutorLease(stateDir, "holder-a", func(lease *fdlExecutorLease) error {
		lease.ActionID = "action-1"
		return writeFDLExecutorJSON(filepath.Join(stateDir, "executor.lease"), lease)
	}); err != nil {
		t.Fatalf("acquire initial lease: %v", err)
	}
	if err := withFDLExecutorLease(stateDir, "holder-b", func(*fdlExecutorLease) error { return nil }); err == nil {
		t.Fatal("different holder acquired a live lease")
	}

	lease, err := readFDLExecutorLease(filepath.Join(stateDir, "executor.lease"))
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	lease.ExpiresAt = time.Now().UTC().Add(-time.Second)
	if err := writeFDLExecutorJSON(filepath.Join(stateDir, "executor.lease"), lease); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := withFDLExecutorLease(stateDir, "holder-b", func(lease *fdlExecutorLease) error {
		if lease.ActionID != "action-1" {
			t.Fatalf("lease action_id = %q, want action-1", lease.ActionID)
		}
		return nil
	}); err != nil {
		t.Fatalf("replacement holder did not recover expired lease: %v", err)
	}

	want := &fdlExecutorMailbox{
		SchemaVersion: 1, FDLRunID: "fdl4-test", ActionID: "action-1", ActionKind: "dispatch_planning_role",
		Envelope: []byte(`{"run_id":"fdl4-test","next_action":{"action_id":"action-1"}}`), UpdatedAt: time.Now().UTC(),
	}
	mailboxPath := filepath.Join(stateDir, "executor.mailbox")
	if err := writeFDLExecutorJSON(mailboxPath, want); err != nil {
		t.Fatalf("write mailbox: %v", err)
	}
	got, err := readFDLExecutorMailbox(mailboxPath)
	if err != nil {
		t.Fatalf("read mailbox: %v", err)
	}
	if got.FDLRunID != want.FDLRunID || got.ActionID != want.ActionID || got.ActionKind != want.ActionKind {
		t.Fatalf("mailbox = %#v, want %#v", got, want)
	}
}

func TestFDLHumanDecisionChoicesFollowControllerCheckpointKind(t *testing.T) {
	tests := []struct {
		kind string
		want []string
		ok   bool
	}{
		{kind: "planning_checkpoint", want: []string{"accept", "reject", "retry", "cancel"}, ok: true},
		{kind: "final_approval", want: []string{"accept", "approve", "reject", "cancel"}, ok: true},
		{kind: "environment_preflight_blocked", want: []string{"retry", "cancel"}, ok: true},
		{kind: "unknown", ok: false},
	}
	for _, tt := range tests {
		got, ok := fdlDecisionChoices(tt.kind)
		if ok != tt.ok || !slices.Equal(got, tt.want) {
			t.Fatalf("fdlDecisionChoices(%q) = (%v, %v), want (%v, %v)", tt.kind, got, ok, tt.want, tt.ok)
		}
	}
}

func TestFDLMailboxRecoveryParsesOnlyCurrentAction(t *testing.T) {
	mailbox := &fdlExecutorMailbox{
		SchemaVersion: 1, FDLRunID: "fdl-run", ActionID: "recover-action", ActionKind: "recover_external_work",
		Envelope: []byte(`{"run_id":"fdl-run","next_action":{"action_id":"recover-action","kind":"recover_external_work","external_work_id":"private-work","work_items":["private-item"]}}`),
	}
	action, err := fdlMailboxRecovery(mailbox)
	if err != nil {
		t.Fatalf("fdlMailboxRecovery: %v", err)
	}
	if action.ExternalWorkID != "private-work" || !slices.Equal(action.WorkItems, []string{"private-item"}) {
		t.Fatalf("recovery action = %#v", action)
	}
	mailbox.ActionID = "different-action"
	if _, err := fdlMailboxRecovery(mailbox); err == nil {
		t.Fatal("mismatched mailbox action was accepted")
	}
}

func TestFDLRecoverRecoveryReceiptFinalizesControllerAcceptedResolution(t *testing.T) {
	completed := false
	projected := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/daemon/fdl-runs/local-run/decisions/decision-1/complete":
			var request struct {
				OperationID string `json:"operation_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode completion request: %v", err)
			}
			if request.OperationID != "recovery-op" {
				t.Fatalf("completion operation = %q, want recovery-op", request.OperationID)
			}
			if runtimeID := r.Header.Get(fdlRuntimeHeader); runtimeID != "runtime-1" {
				t.Fatalf("FDL runtime identity = %q, want runtime-1", runtimeID)
			}
			completed = true
		case "/api/daemon/fdl-runs/local-run/projection":
			var projection FDLProjection
			if err := json.NewDecoder(r.Body).Decode(&projection); err != nil {
				t.Fatalf("decode recovery projection: %v", err)
			}
			if projection.Status != "running" || projection.Phase != "design" || projection.ActionType != "dispatch_planning_role" {
				t.Fatalf("recovery projection = %#v", projection)
			}
			projected = true
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	root := t.TempDir()
	d := &Daemon{
		cfg:    Config{FDLRunRoot: filepath.Join(root, "fdl-runs")},
		client: NewClient(server.URL),
	}
	stateDir := d.fdlExecutorStateDir("local-run")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create executor state: %v", err)
	}
	returnedEnvelope := json.RawMessage(`{"run_id":"controller-run","state":{"phase":"planning"},"next_action":{"action_id":"next-action","kind":"dispatch_planning_role"}}`)
	receipt := fdlRecoveryReceipt{
		SchemaVersion: 1, DecisionID: "decision-1", FDLRunID: "controller-run", ActionID: "recover-action",
		ExternalWorkID: "private-work", WorkItemID: "private-item", SubmissionToken: "private-token",
		Resolution: "retry", OperationID: "recovery-op", ControllerSubmitted: true, ReturnedEnvelope: returnedEnvelope,
	}
	if err := writeFDLExecutorJSON(d.recoveryReceiptPath("local-run"), receipt); err != nil {
		t.Fatalf("write recovery receipt: %v", err)
	}
	mailbox := &fdlExecutorMailbox{
		SchemaVersion: 1, FDLRunID: "controller-run", ActionID: "recover-action", ActionKind: "recover_external_work",
		Envelope: json.RawMessage(`{"run_id":"controller-run","next_action":{"action_id":"recover-action","kind":"recover_external_work"}}`),
	}

	if err := d.recoverFDLRecoveryReceipt(context.Background(), "local-run", "runtime-1", mailbox); err != nil {
		t.Fatalf("recover FDL recovery receipt: %v", err)
	}
	if !completed {
		t.Fatal("expected daemon to complete the already accepted recovery decision")
	}
	if !projected {
		t.Fatal("expected daemon to restore the running projection before redispatch")
	}
	if _, err := os.Stat(d.recoveryReceiptPath("local-run")); !os.IsNotExist(err) {
		t.Fatalf("recovery receipt still exists after finalization: %v", err)
	}
	next, err := readFDLExecutorMailbox(filepath.Join(stateDir, "executor.mailbox"))
	if err != nil {
		t.Fatalf("read advanced mailbox: %v", err)
	}
	if next == nil || next.ActionID != "next-action" || next.ActionKind != "dispatch_planning_role" {
		t.Fatalf("mailbox was not advanced to Controller reply: %#v", next)
	}
}

func TestFDLRunningProjectionForDispatchIgnoresNonDispatchReplies(t *testing.T) {
	projection, dispatch, err := fdlRunningProjectionForDispatch(json.RawMessage(`{"state":{"phase":"review"},"next_action":{"kind":"handoff_external"}}`))
	if err != nil || dispatch || projection.Status != "" {
		t.Fatalf("handoff reply projection = (%#v, %v, %v)", projection, dispatch, err)
	}
}

func TestFDLBindingArchiveEligibilityRequiresControllerTerminalState(t *testing.T) {
	tests := []struct {
		name    string
		binding fdlWorkItemBinding
		want    bool
	}{
		{name: "submitted result", binding: fdlWorkItemBinding{State: "terminal_submitted", TerminalSubmitted: true}, want: true},
		{name: "acknowledged dispatch failure", binding: fdlWorkItemBinding{State: "dispatch_failed"}, want: true},
		{name: "prepared", binding: fdlWorkItemBinding{State: "prepared"}, want: false},
		{name: "pending acknowledgement", binding: fdlWorkItemBinding{State: "pending_ack"}, want: false},
		{name: "activated", binding: fdlWorkItemBinding{State: "activated"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fdlBindingCanBeArchived(tt.binding); got != tt.want {
				t.Fatalf("fdlBindingCanBeArchived(%#v) = %v, want %v", tt.binding, got, tt.want)
			}
		})
	}
}
