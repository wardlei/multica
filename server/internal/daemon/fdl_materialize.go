package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// fdlExecutorWorktree is local executor state, not Controller state. It gives
// materialization a frozen source root without putting an FDL run-root path in
// an Agent task or in the database.
type fdlExecutorWorktree struct {
	SchemaVersion int    `json:"schema_version"`
	LocalPath     string `json:"local_path"`
}

type fdlEvidenceReference struct {
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	SchemaVersion int    `json:"schema_version"`
}

type fdlCodeSnapshot struct {
	SchemaVersion int    `json:"schema_version"`
	Head          string `json:"head"`
	Branch        string `json:"branch"`
	Changes       []struct {
		Path   string `json:"path"`
		Change string `json:"change"`
		Mode   string `json:"mode"`
		SHA256 string `json:"sha256"`
	} `json:"changes"`
}

// materializeFDLExecutionWorkspace provides the only inputs reviewers and
// explorers may read. It never points those roles at the live worktree.
func (d *Daemon) materializeFDLExecutionWorkspace(runID string, binding fdlWorkItemBinding, payload fdlDispatchPayload) (json.RawMessage, error) {
	role, err := fdlProfileRole(payload)
	if err != nil {
		return nil, err
	}
	if role != "explorer:impact_analysis" && !strings.HasPrefix(role, "reviewer:") {
		return nil, nil
	}
	worktree, err := d.readFDLExecutorWorktree(runID)
	if err != nil {
		return nil, err
	}
	target, err := d.fdlIsolatedWorkspacePath(runID, binding.WorkItemID)
	if err != nil {
		return nil, err
	}
	if role == "explorer:impact_analysis" {
		if err := materializeFDLExplorerProjection(worktree.LocalPath, payload.Instructions, target); err != nil {
			return nil, err
		}
	} else if err := d.materializeFDLReviewSnapshot(runID, worktree.LocalPath, payload, target); err != nil {
		return nil, err
	}
	canonical, err := resolveRealPath(target)
	if err != nil {
		return nil, fmt.Errorf("resolve isolated FDL workspace: %w", err)
	}
	return json.Marshal(fdlIsolatedWorkspaceContext{
		SchemaVersion: 1, Kind: "fdl_isolated_workspace", DaemonID: d.cfg.DaemonID,
		LocalPath: target, CanonicalPath: canonical, ReadOnly: true,
	})
}

func (d *Daemon) readFDLExecutorWorktree(runID string) (fdlExecutorWorktree, error) {
	var worktree fdlExecutorWorktree
	found, err := readFDLExecutorJSON(filepath.Join(d.fdlExecutorStateDir(runID), "worktree.json"), &worktree)
	if err != nil || !found || worktree.SchemaVersion != 1 || !filepath.IsAbs(worktree.LocalPath) {
		return fdlExecutorWorktree{}, fmt.Errorf("frozen FDL source worktree binding is unavailable")
	}
	if err := validateLocalPath(worktree.LocalPath); err != nil {
		return fdlExecutorWorktree{}, fmt.Errorf("validate frozen FDL source worktree: %w", err)
	}
	return worktree, nil
}

func (d *Daemon) fdlIsolatedWorkspacePath(runID, workItemID string) (string, error) {
	if strings.TrimSpace(runID) == "" || !fdlSafePathSegment(workItemID) {
		return "", fmt.Errorf("invalid FDL isolated workspace identity")
	}
	if !filepath.IsAbs(d.cfg.WorkspacesRoot) {
		return "", fmt.Errorf("FDL isolated workspace root must be absolute")
	}
	root := filepath.Join(d.cfg.WorkspacesRoot, "fdl-isolated")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create FDL isolated workspace root: %w", err)
	}
	digest := sha256.Sum256([]byte(runID + "\x00" + workItemID))
	return filepath.Join(root, hex.EncodeToString(digest[:16])), nil
}

func fdlSafePathSegment(value string) bool {
	if value == "" || len(value) > 160 || strings.ContainsAny(value, `/\\`) || strings.Contains(value, "..") {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (d *Daemon) materializeFDLReviewSnapshot(runID, sourceRoot string, payload fdlDispatchPayload, target string) error {
	inputHash := payload.InputHashes["code_snapshot_hash"]
	implementationRaw, ok := payload.InputEvidence["implementation"]
	if inputHash == "" || !ok {
		return fmt.Errorf("FDL review dispatch has no implementation snapshot binding")
	}
	var implementation struct {
		CodeSnapshot fdlEvidenceReference `json:"code_snapshot"`
	}
	if json.Unmarshal(implementationRaw, &implementation) != nil || implementation.CodeSnapshot.Path == "" || implementation.CodeSnapshot.SHA256 == "" {
		return fmt.Errorf("FDL review implementation evidence is invalid")
	}
	rawSnapshot, err := d.readFDLEvidence(runID, implementation.CodeSnapshot)
	if err != nil {
		return err
	}
	var rawValue any
	var snapshot fdlCodeSnapshot
	if json.Unmarshal(rawSnapshot, &rawValue) != nil || json.Unmarshal(rawSnapshot, &snapshot) != nil || snapshot.SchemaVersion != 3 || len(snapshot.Head) != 40 || snapshot.Branch == "" {
		return fmt.Errorf("FDL code snapshot is invalid")
	}
	canonical, err := canonicalFDLJSON(rawValue)
	if err != nil || "sha256:"+hexDigest(canonical) != inputHash {
		return fmt.Errorf("FDL code snapshot hash does not match review input")
	}
	if err := resetFDLIsolatedDirectory(target); err != nil {
		return err
	}
	if err := extractFDLGitArchive(sourceRoot, snapshot.Head, target); err != nil {
		_ = removeFDLIsolatedDirectory(target)
		return err
	}
	for _, change := range snapshot.Changes {
		if (change.Change != "A" && change.Change != "M") || (change.Mode != "100644" && change.Mode != "100755") || !fdlSafeRelativePath(change.Path) || !strings.HasPrefix(change.SHA256, "sha256:") {
			_ = removeFDLIsolatedDirectory(target)
			return fmt.Errorf("FDL snapshot change is invalid")
		}
		if err := copyFDLVerifiedFile(sourceRoot, target, change.Path, change.SHA256, change.Mode); err != nil {
			_ = removeFDLIsolatedDirectory(target)
			return err
		}
	}
	if err := freezeFDLDirectory(target); err != nil {
		_ = removeFDLIsolatedDirectory(target)
		return err
	}
	return nil
}

func materializeFDLExplorerProjection(sourceRoot string, instructions json.RawMessage, target string) error {
	var request struct {
		AllowedPaths []string `json:"allowed_paths"`
	}
	if json.Unmarshal(instructions, &request) != nil || len(request.AllowedPaths) == 0 {
		return fmt.Errorf("FDL Explorer dispatch has no valid allowlist")
	}
	if err := resetFDLIsolatedDirectory(target); err != nil {
		return err
	}
	for _, relative := range request.AllowedPaths {
		if !fdlSafeRelativePath(relative) {
			_ = removeFDLIsolatedDirectory(target)
			return fmt.Errorf("FDL Explorer allowlist path is unsafe")
		}
		if err := copyFDLProjectionPath(sourceRoot, target, relative); err != nil {
			_ = removeFDLIsolatedDirectory(target)
			return err
		}
	}
	if err := freezeFDLDirectory(target); err != nil {
		_ = removeFDLIsolatedDirectory(target)
		return err
	}
	return nil
}

func (d *Daemon) readFDLEvidence(runID string, ref fdlEvidenceReference) ([]byte, error) {
	if ref.SchemaVersion != 1 || ref.Path == "" || !strings.HasPrefix(ref.SHA256, "sha256:") {
		return nil, fmt.Errorf("FDL evidence reference is invalid")
	}
	path, err := fdlRunFilePath(filepath.Join(d.cfg.FDLRunRoot, runID), ref.Path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read FDL evidence: %w", err)
	}
	if "sha256:"+hexDigest(data) != ref.SHA256 {
		return nil, fmt.Errorf("FDL evidence hash mismatch")
	}
	return data, nil
}

func extractFDLGitArchive(sourceRoot, head, target string) error {
	archive := exec.Command("git", "-C", sourceRoot, "archive", head)
	data, err := archive.Output()
	if err != nil {
		return fmt.Errorf("materialize FDL review snapshot: git archive %s: %w", head, err)
	}
	untar := exec.Command("tar", "-x", "-C", target)
	untar.Stdin = bytes.NewReader(data)
	if output, err := untar.CombinedOutput(); err != nil {
		return fmt.Errorf("materialize FDL review snapshot: extract archive: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func copyFDLVerifiedFile(sourceRoot, target, relative, expectedHash, mode string) error {
	source, err := fdlSourcePath(sourceRoot, relative)
	if err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("FDL snapshot source is not a regular file: %q", relative)
	}
	data, err := os.ReadFile(source)
	if err != nil || "sha256:"+hexDigest(data) != expectedHash {
		return fmt.Errorf("FDL snapshot source content mismatch: %q", relative)
	}
	destination := filepath.Join(target, relative)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(destination, data, map[bool]os.FileMode{true: 0o755, false: 0o644}[mode == "100755"]); err != nil {
		return err
	}
	return nil
}

func copyFDLProjectionPath(sourceRoot, target, relative string) error {
	source, err := fdlSourcePath(sourceRoot, relative)
	if err != nil {
		return err
	}
	destination := filepath.Join(target, relative)
	return copyFDLProjectionEntry(source, destination)
}

func copyFDLProjectionEntry(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("FDL Explorer allowlist source is missing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("FDL Explorer source contains symlink: %q", source)
	}
	if info.Mode().IsRegular() {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o644)
	}
	if !info.IsDir() {
		return fmt.Errorf("FDL Explorer source is not a regular file or directory: %q", source)
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyFDLProjectionEntry(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func fdlSourcePath(root, relative string) (string, error) {
	if !fdlSafeRelativePath(relative) {
		return "", fmt.Errorf("unsafe FDL source path")
	}
	path := root
	for _, part := range strings.Split(relative, "/") {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("read FDL source path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("FDL source path contains symlink: %q", relative)
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve FDL source path: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || !strings.HasPrefix(resolved, canonicalRoot+string(filepath.Separator)) && resolved != canonicalRoot {
		return "", fmt.Errorf("FDL source path escapes frozen worktree")
	}
	return path, nil
}

func fdlSafeRelativePath(value string) bool {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, `\`) || filepath.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func resetFDLIsolatedDirectory(target string) error {
	if err := removeFDLIsolatedDirectory(target); err != nil {
		return err
	}
	return os.MkdirAll(target, 0o755)
}

func removeFDLIsolatedDirectory(target string) error {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return nil
	}
	if err := filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode()&os.ModeSymlink == 0 {
			return os.Chmod(path, 0o755)
		}
		return err
	}); err != nil {
		return err
	}
	return os.RemoveAll(target)
}

func freezeFDLDirectory(target string) error {
	return filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("FDL isolated workspace contains symlink")
		}
		if info.IsDir() {
			return os.Chmod(path, 0o555)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("FDL isolated workspace contains special file")
		}
		return os.Chmod(path, 0o444)
	})
}

func hexDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
