package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeFDLReviewWorkspaceUsesFrozenSnapshot(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	initFDLMaterializeGitRepo(t, source, map[string]string{"README.md": "base\n", "unrelated.txt": "hidden\n"})
	head := fdlGitOutput(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("review snapshot\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{cfg: Config{DaemonID: "daemon-fdl", WorkspacesRoot: filepath.Join(root, "workspaces"), FDLRunRoot: filepath.Join(root, "runs")}}
	runID := "run-review"
	isolationPath, err := d.fdlIsolatedWorkspacePath(runID, "reviewer_correctness")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeFDLIsolatedDirectory(isolationPath) })
	if err := os.MkdirAll(d.fdlExecutorStateDir(runID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "worktree.json"), fdlExecutorWorktree{SchemaVersion: 1, LocalPath: source}); err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]any{
		"schema_version": 4,
		"head":           head,
		"branch":         "main",
		"changes": []any{map[string]any{
			"path": "README.md", "change": "M", "mode": "100644", "sha256": "sha256:" + hexDigest([]byte("review snapshot\n")),
		}},
	}
	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(d.cfg.FDLRunRoot, runID, "snapshots", "implementation.json")
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, rawSnapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(rawSnapshot, &decoded); err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalFDLJSON(decoded)
	if err != nil {
		t.Fatal(err)
	}
	implementation, _ := json.Marshal(map[string]any{"code_snapshot": map[string]any{
		"schema_version": 4, "path": "snapshots/implementation.json", "sha256": "sha256:" + hexDigest(rawSnapshot),
	}})
	payload := fdlDispatchPayload{InputHashes: map[string]string{"code_snapshot_hash": "sha256:" + hexDigest(canonical)}, InputEvidence: map[string]json.RawMessage{"implementation": implementation}}
	payload.Attempt.Role = "feature-delivery-reviewer"
	payload.Instructions = json.RawMessage(`{"lane":"correctness"}`)
	workspace, err := d.materializeFDLExecutionWorkspace(runID, fdlWorkItemBinding{WorkItemID: "reviewer_correctness"}, payload)
	if err != nil {
		t.Fatalf("materialize FDL review workspace: %v", err)
	}
	var context fdlIsolatedWorkspaceContext
	if err := json.Unmarshal(workspace, &context); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(context.LocalPath, "README.md"))
	if err != nil || string(got) != "review snapshot\n" {
		t.Fatalf("review snapshot contents = %q, err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(context.LocalPath, "unrelated.txt")); err != nil {
		t.Fatalf("git archive base file missing from review snapshot: %v", err)
	}
	if info, err := os.Stat(filepath.Join(context.LocalPath, "README.md")); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("review snapshot is writable: mode=%v err=%v", info.Mode(), err)
	}
	assignment, err := codeWorktreeAssignmentForTask(Task{WorktreeContext: workspace}, d.cfg.DaemonID)
	if err != nil || assignment == nil || !assignment.ReadOnly || assignment.AbsPath != context.LocalPath {
		t.Fatalf("isolated workspace assignment = %#v, err=%v", assignment, err)
	}
}

func TestMaterializeFDLExplorerWorkspaceCopiesOnlyAllowlist(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "src", "visible.go"), []byte("package src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "secret.txt"), []byte("private\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{DaemonID: "daemon-fdl", WorkspacesRoot: filepath.Join(root, "workspaces"), FDLRunRoot: filepath.Join(root, "runs")}}
	runID := "run-explorer"
	isolationPath, err := d.fdlIsolatedWorkspacePath(runID, "explorer_impact")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeFDLIsolatedDirectory(isolationPath) })
	if err := os.MkdirAll(d.fdlExecutorStateDir(runID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "worktree.json"), fdlExecutorWorktree{SchemaVersion: 1, LocalPath: source}); err != nil {
		t.Fatal(err)
	}
	payload := fdlDispatchPayload{Instructions: json.RawMessage(`{"allowed_paths":["src/visible.go"]}`)}
	payload.Attempt.Role = "explorer"
	workspace, err := d.materializeFDLExecutionWorkspace(runID, fdlWorkItemBinding{WorkItemID: "explorer_impact"}, payload)
	if err != nil {
		t.Fatalf("materialize FDL Explorer workspace: %v", err)
	}
	var context fdlIsolatedWorkspaceContext
	if err := json.Unmarshal(workspace, &context); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(context.LocalPath, "src", "visible.go")); err != nil {
		t.Fatalf("allowlisted source is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(context.LocalPath, "secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("Explorer projection leaked non-allowlisted file: %v", err)
	}
	if info, err := os.Stat(context.LocalPath); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("Explorer projection is writable: mode=%v err=%v", info.Mode(), err)
	}
}

func TestFDLIsolatedWorkspaceRejectsWritableTree(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mutable.txt"), []byte("mutable"), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical, err := resolveRealPath(root)
	if err != nil {
		t.Fatal(err)
	}
	context, err := json.Marshal(fdlIsolatedWorkspaceContext{SchemaVersion: 1, Kind: "fdl_isolated_workspace", DaemonID: "daemon-fdl", LocalPath: root, CanonicalPath: canonical, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codeWorktreeAssignmentForTask(Task{WorktreeContext: context}, "daemon-fdl"); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("writable FDL isolated workspace was accepted: %v", err)
	}
}

func TestFDLReadOnlyWorkspaceSkipsWriteProbe(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "snapshot.txt"), []byte("frozen\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	canonical, err := resolveRealPath(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := json.Marshal(fdlIsolatedWorkspaceContext{SchemaVersion: 1, Kind: "fdl_isolated_workspace", DaemonID: "daemon-fdl", LocalPath: root, CanonicalPath: canonical, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{DaemonID: "daemon-fdl"}, localPathLocks: NewLocalPathLocker()}
	release, abort := d.acquireLocalDirectoryLockIfNeeded(context.Background(), Task{ID: "fdl-readonly", WorktreeContext: workspace}, slog.Default())
	if abort || release == nil {
		t.Fatalf("read-only FDL workspace was rejected: abort=%v release=%v", abort, release != nil)
	}
	release()
}

func TestMaterializeFDLExplorerWorkspaceRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "src", "escape.txt")); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{DaemonID: "daemon-fdl", WorkspacesRoot: filepath.Join(root, "workspaces"), FDLRunRoot: filepath.Join(root, "runs")}}
	runID := "run-explorer-symlink"
	if err := os.MkdirAll(d.fdlExecutorStateDir(runID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "worktree.json"), fdlExecutorWorktree{SchemaVersion: 1, LocalPath: source}); err != nil {
		t.Fatal(err)
	}
	target, err := d.fdlIsolatedWorkspacePath(runID, "explorer_symlink")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeFDLIsolatedDirectory(target) })
	payload := fdlDispatchPayload{Instructions: json.RawMessage(`{"allowed_paths":["src/escape.txt"]}`)}
	payload.Attempt.Role = "explorer"
	if _, err := d.materializeFDLExecutionWorkspace(runID, fdlWorkItemBinding{WorkItemID: "explorer_symlink"}, payload); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Explorer materialization accepted a symlink: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("failed Explorer materialization left a projection behind: %v", err)
	}
}

func initFDLMaterializeGitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fdlGit(t, dir, "init", "-b", "main")
	fdlGit(t, dir, "config", "user.email", "fdl@example.test")
	fdlGit(t, dir, "config", "user.name", "FDL Test")
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fdlGit(t, dir, "add", ".")
	fdlGit(t, dir, "commit", "-m", "initial")
}

func fdlGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func fdlGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}
