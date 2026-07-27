import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { projectKeys } from "./queries";
import type {
  CreateCodeWorktreeRequest,
  RefreshCodeWorktreeRequest,
  SetProjectCodeWorktreeRequest,
} from "../types";

export const codeWorktreeKeys = {
  all: (wsId: string) => ["code-worktrees", wsId] as const,
  list: (wsId: string) => [...codeWorktreeKeys.all(wsId), "list"] as const,
};

export function codeWorktreesOptions(wsId: string) {
  return queryOptions({
    queryKey: codeWorktreeKeys.list(wsId),
    queryFn: () => api.listCodeWorktrees(),
    select: (data) => data.worktrees,
  });
}

function invalidateProjectAndWorktrees(
  qc: ReturnType<typeof useQueryClient>,
  wsId: string,
  projectId?: string,
) {
  qc.invalidateQueries({ queryKey: codeWorktreeKeys.list(wsId) });
  if (projectId) {
    qc.invalidateQueries({ queryKey: projectKeys.detail(wsId, projectId) });
  }
  qc.invalidateQueries({ queryKey: projectKeys.list(wsId) });
}

export function useCreateCodeWorktree(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: CreateCodeWorktreeRequest) => api.createCodeWorktree(data),
    onSettled: () => invalidateProjectAndWorktrees(qc, wsId),
  });
}

export function useRefreshCodeWorktree(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: RefreshCodeWorktreeRequest }) =>
      api.refreshCodeWorktree(id, data),
    onSettled: () => invalidateProjectAndWorktrees(qc, wsId),
  });
}

export function useDeleteCodeWorktree(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.deleteCodeWorktree(id),
    onSettled: () => invalidateProjectAndWorktrees(qc, wsId),
  });
}

export function useSetProjectCodeWorktree(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: SetProjectCodeWorktreeRequest) =>
      api.setProjectCodeWorktree(projectId, data),
    onSettled: () => invalidateProjectAndWorktrees(qc, wsId, projectId),
  });
}
