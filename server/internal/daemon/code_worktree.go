package daemon

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type codeWorktreeInspection struct {
	CanonicalPath string `json:"canonical_path"`
	RepositoryURL string `json:"repository_url"`
	Branch        string `json:"branch"`
	HeadSHA       string `json:"head_sha"`
	IsDirty       bool   `json:"is_dirty"`
}

func inspectCodeWorktree(localPath string) (codeWorktreeInspection, error) {
	absPath, err := normalizeLocalPath(localPath)
	if err != nil {
		return codeWorktreeInspection{}, err
	}
	if err := validateLocalPath(absPath); err != nil {
		return codeWorktreeInspection{}, err
	}
	canonical, err := resolveRealPath(absPath)
	if err != nil {
		return codeWorktreeInspection{}, err
	}
	git := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", absPath}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	remote, err := git("config", "--get", "remote.origin.url")
	if err != nil || remote == "" {
		return codeWorktreeInspection{}, fmt.Errorf("origin remote is required")
	}
	branch, err := git("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch == "" {
		return codeWorktreeInspection{}, fmt.Errorf("a non-detached branch is required")
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil || len(head) != 40 {
		return codeWorktreeInspection{}, fmt.Errorf("read HEAD: %w", err)
	}
	dirty, err := git("status", "--porcelain=v1")
	if err != nil {
		return codeWorktreeInspection{}, fmt.Errorf("read Git status: %w", err)
	}
	return codeWorktreeInspection{CanonicalPath: canonical, RepositoryURL: normalizeRepositoryURL(remote), Branch: branch, HeadSHA: head, IsDirty: dirty != ""}, nil
}

type codeWorktreeContext struct {
	WorktreeID      string `json:"worktree_id"`
	DaemonID        string `json:"daemon_id"`
	LocalPath       string `json:"local_path"`
	CanonicalPath   string `json:"canonical_path"`
	RepositoryURL   string `json:"repository_url"`
	ExpectedBranch  string `json:"expected_branch"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
	MustBeClean     bool   `json:"must_be_clean"`
}

// fdlIsolatedWorkspaceContext is a daemon-local execution projection. Unlike
// codeWorktreeContext it deliberately has no Git identity: reviewers and
// explorers receive a frozen materialization, not the live repository.
type fdlIsolatedWorkspaceContext struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	DaemonID      string `json:"daemon_id"`
	LocalPath     string `json:"local_path"`
	CanonicalPath string `json:"canonical_path"`
	ReadOnly      bool   `json:"read_only"`
}

func codeWorktreeAssignmentForTask(task Task, daemonID string) (*localDirectoryAssignment, error) {
	if len(task.WorktreeContext) == 0 || string(task.WorktreeContext) == "{}" {
		return localDirectoryAssignmentForTask(task, daemonID)
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(task.WorktreeContext, &kind); err != nil {
		return nil, fmt.Errorf("worktree_preflight_error: parse task context: %w", err)
	}
	if kind.Kind == "fdl_isolated_workspace" {
		return fdlIsolatedWorkspaceAssignment(task.WorktreeContext, daemonID)
	}
	var expected codeWorktreeContext
	if err := json.Unmarshal(task.WorktreeContext, &expected); err != nil {
		return nil, fmt.Errorf("worktree_preflight_error: parse task context: %w", err)
	}
	if expected.DaemonID == "" || expected.DaemonID != daemonID {
		return nil, fmt.Errorf("worktree_preflight_error: task belongs to daemon %q, not this daemon", expected.DaemonID)
	}
	absPath, err := normalizeLocalPath(expected.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("worktree_preflight_error: %w", err)
	}
	realPath, err := resolveRealPath(absPath)
	if err != nil {
		return nil, fmt.Errorf("worktree_preflight_error: %w", err)
	}
	if err := validateLocalPath(absPath); err != nil {
		return nil, fmt.Errorf("worktree_preflight_error: %w", err)
	}
	if realPath != expected.CanonicalPath {
		return nil, fmt.Errorf("worktree_preflight_error: canonical path drift: got %q, want %q", realPath, expected.CanonicalPath)
	}
	if task.FDLDirect {
		// The FDL Controller, not a new direct task, owns the implementation
		// snapshot. A recovery task may therefore resume an allowed dirty
		// snapshot that the Controller has not accepted yet.
		expected.MustBeClean = false
	}
	if err := verifyCodeWorktree(absPath, expected); err != nil {
		return nil, err
	}
	return &localDirectoryAssignment{AbsPath: absPath, RealPath: realPath}, nil
}

func fdlIsolatedWorkspaceAssignment(raw json.RawMessage, daemonID string) (*localDirectoryAssignment, error) {
	var expected fdlIsolatedWorkspaceContext
	if err := json.Unmarshal(raw, &expected); err != nil {
		return nil, fmt.Errorf("worktree_preflight_error: parse FDL isolated workspace: %w", err)
	}
	if expected.SchemaVersion != 1 || expected.Kind != "fdl_isolated_workspace" || !expected.ReadOnly || expected.DaemonID == "" || expected.DaemonID != daemonID {
		return nil, fmt.Errorf("worktree_preflight_error: invalid FDL isolated workspace contract")
	}
	absPath, err := normalizeLocalPath(expected.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("worktree_preflight_error: %w", err)
	}
	realPath, err := resolveRealPath(absPath)
	if err != nil {
		return nil, fmt.Errorf("worktree_preflight_error: %w", err)
	}
	if realPath != expected.CanonicalPath {
		return nil, fmt.Errorf("worktree_preflight_error: FDL isolated workspace drift: got %q, want %q", realPath, expected.CanonicalPath)
	}
	if err := verifyFDLReadOnlyWorkspace(absPath); err != nil {
		return nil, err
	}
	return &localDirectoryAssignment{AbsPath: absPath, RealPath: realPath, ReadOnly: true}, nil
}

func verifyFDLReadOnlyWorkspace(root string) error {
	if reason, blocked := isBlacklistedLocalPath(root); blocked {
		return fmt.Errorf("worktree_preflight_error: FDL isolated workspace %s", reason)
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("worktree_preflight_error: read FDL isolated workspace: %w", err)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("worktree_preflight_error: FDL isolated workspace contains symlink %q", path)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("worktree_preflight_error: inspect FDL isolated workspace: %w", err)
		}
		if info.Mode().Perm()&0o222 != 0 {
			return fmt.Errorf("worktree_preflight_error: FDL isolated workspace is writable at %q", path)
		}
		return nil
	})
}

func verifyCodeWorktree(dir string, expected codeWorktreeContext) error {
	git := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	remote, err := git("config", "--get", "remote.origin.url")
	if err != nil || normalizeRepositoryURL(remote) != normalizeRepositoryURL(expected.RepositoryURL) {
		return fmt.Errorf("worktree_preflight_error: origin drift: got %q, want %q", normalizeRepositoryURL(remote), normalizeRepositoryURL(expected.RepositoryURL))
	}
	branch, err := git("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != expected.ExpectedBranch {
		return fmt.Errorf("worktree_preflight_error: branch drift: got %q, want %q", branch, expected.ExpectedBranch)
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil || head != expected.ExpectedHeadSHA {
		return fmt.Errorf("worktree_preflight_error: HEAD drift: got %q, want %q", head, expected.ExpectedHeadSHA)
	}
	if expected.MustBeClean {
		dirty, err := git("status", "--porcelain=v1")
		if err != nil || dirty != "" {
			return fmt.Errorf("worktree_preflight_error: worktree has uncommitted changes")
		}
	}
	return nil
}

// normalizeRepositoryURL identifies a Git remote without depending on its
// transport syntax. A user can switch between SSH and HTTPS representations of
// the same repository without making an already-bound worktree unsafe.
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
