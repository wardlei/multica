package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WorktreeContext is copied into an issue task so a later Project edit cannot
// redirect work that was already queued.
type WorktreeContext struct {
	WorktreeID      string `json:"worktree_id"`
	DaemonID        string `json:"daemon_id"`
	LocalPath       string `json:"local_path"`
	CanonicalPath   string `json:"canonical_path"`
	RepositoryURL   string `json:"repository_url"`
	ExpectedBranch  string `json:"expected_branch"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
	MustBeClean     bool   `json:"must_be_clean"`
}

func (s *TaskService) worktreeContextForIssue(ctx context.Context, issue db.Issue, runtimeID pgtype.UUID) ([]byte, error) {
	if !issue.ProjectID.Valid {
		return nil, nil
	}
	worktree, err := s.Queries.GetProjectCodeWorktree(ctx, db.GetProjectCodeWorktreeParams{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("load project code worktree: %w", err)
	}
	if worktree.IsDirty {
		return nil, fmt.Errorf("project code worktree is dirty; refresh it after resolving local changes")
	}
	runtime, err := s.Queries.GetAgentRuntime(ctx, runtimeID)
	if err != nil {
		return nil, fmt.Errorf("load task runtime: %w", err)
	}
	if !runtime.DaemonID.Valid || runtime.DaemonID.String != worktree.DaemonID {
		return nil, fmt.Errorf("agent runtime belongs to a different daemon than the project's code worktree")
	}
	return json.Marshal(WorktreeContext{
		WorktreeID: worktree.ID.String(), DaemonID: worktree.DaemonID,
		LocalPath: worktree.LocalPath, CanonicalPath: worktree.CanonicalPath,
		RepositoryURL: worktree.RepositoryUrl, ExpectedBranch: worktree.Branch,
		ExpectedHeadSHA: worktree.HeadSha, MustBeClean: true,
	})
}
