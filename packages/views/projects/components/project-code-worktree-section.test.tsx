import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { ProjectCodeWorktreeSection } from "./project-code-worktree-section";

const worktree = {
  id: "worktree-1",
  workspace_id: "workspace-1",
  daemon_id: "daemon-1",
  local_path: "/src/multica",
  canonical_path: "/src/multica",
  repository_url: "github.com/multica-ai/multica",
  branch: "feature/worktree",
  head_sha: "0123456789012345678901234567890123456789",
  is_dirty: false,
  inspected_at: "2026-07-28T00:00:00Z",
  updated_at: "2026-07-28T00:00:00Z",
};

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: [worktree] }),
}));

vi.mock("@multica/core/projects", () => ({
  codeWorktreesOptions: () => ({ queryKey: ["code-worktrees"] }),
  useCreateCodeWorktree: () => ({ mutateAsync: vi.fn() }),
  useRefreshCodeWorktree: () => ({ mutateAsync: vi.fn() }),
  useSetProjectCodeWorktree: () => ({ mutate: vi.fn(), mutateAsync: vi.fn() }),
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));

vi.mock("../../platform", () => ({
  isDesktopShell: () => false,
  pickDirectory: vi.fn(),
  inspectLocalCodeWorktree: vi.fn(),
  useLocalDaemonStatus: () => ({ running: false, daemonId: null }),
}));

describe("ProjectCodeWorktreeSection", () => {
  it("shows a bound checkout as desktop-only read-only context on the web", () => {
    renderWithI18n(
      <ProjectCodeWorktreeSection projectId="project-1" defaultCodeWorktreeId="worktree-1" />,
    );

    expect(screen.getByText("Code worktree")).toBeInTheDocument();
    expect(screen.getByText("/src/multica")).toBeInTheDocument();
    expect(screen.getByText(/feature\/worktree/)).toBeInTheDocument();
    expect(screen.getByText(/only be changed from the desktop app/i)).toBeInTheDocument();
  });
});
