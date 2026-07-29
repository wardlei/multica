package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInspectCodeWorktreeAndPreflight(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	run("remote", "add", "origin", "git@github.com:example/repo.git")

	inspection, err := inspectCodeWorktree(dir)
	if err != nil {
		t.Fatalf("inspectCodeWorktree: %v", err)
	}
	if inspection.IsDirty || inspection.Branch != "main" || inspection.HeadSHA == "" {
		t.Fatalf("unexpected inspection: %+v", inspection)
	}
	context := codeWorktreeContext{DaemonID: "daemon-a", LocalPath: dir, CanonicalPath: inspection.CanonicalPath, RepositoryURL: inspection.RepositoryURL, ExpectedBranch: inspection.Branch, ExpectedHeadSHA: inspection.HeadSHA, MustBeClean: true}
	if err := verifyCodeWorktree(dir, context); err != nil {
		t.Fatalf("verifyCodeWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyCodeWorktree(dir, context); err == nil {
		t.Fatal("expected dirty worktree preflight failure")
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := codeWorktreeAssignmentForTask(Task{FDLDirect: true, WorktreeContext: encoded}, "daemon-a")
	if err != nil || assignment == nil || assignment.AbsPath != dir {
		t.Fatalf("FDL direct task did not accept Controller-owned dirty snapshot: assignment=%#v err=%v", assignment, err)
	}
}
