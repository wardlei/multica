package daemon

import (
	"encoding/json"
	"path/filepath"
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
