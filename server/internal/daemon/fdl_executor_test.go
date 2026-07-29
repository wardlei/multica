package daemon

import (
	"context"
	"encoding/json"
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

func TestReadFDLRoleMetaRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := writeFDLExecutorJSON(path, map[string]any{"outcome": "completed", "token": "must-not-pass"}); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if _, err := readFDLRoleMeta(path); err == nil {
		t.Fatal("metadata with an unknown field was accepted")
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/daemon/fdl-runs/local-run/decisions/decision-1/complete" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var request struct {
			OperationID string `json:"operation_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode completion request: %v", err)
		}
		if request.OperationID != "recovery-op" {
			t.Fatalf("completion operation = %q, want recovery-op", request.OperationID)
		}
		completed = true
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
	returnedEnvelope := json.RawMessage(`{"run_id":"controller-run","next_action":{"action_id":"next-action","kind":"await_external_results"}}`)
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

	if err := d.recoverFDLRecoveryReceipt(context.Background(), "local-run", mailbox); err != nil {
		t.Fatalf("recover FDL recovery receipt: %v", err)
	}
	if !completed {
		t.Fatal("expected daemon to complete the already accepted recovery decision")
	}
	if _, err := os.Stat(d.recoveryReceiptPath("local-run")); !os.IsNotExist(err) {
		t.Fatalf("recovery receipt still exists after finalization: %v", err)
	}
	next, err := readFDLExecutorMailbox(filepath.Join(stateDir, "executor.mailbox"))
	if err != nil {
		t.Fatalf("read advanced mailbox: %v", err)
	}
	if next == nil || next.ActionID != "next-action" || next.ActionKind != "await_external_results" {
		t.Fatalf("mailbox was not advanced to Controller reply: %#v", next)
	}
}
