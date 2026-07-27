"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { FolderGit, RefreshCw, Unlink } from "lucide-react";
import { toast } from "sonner";
import {
  codeWorktreesOptions,
  useCreateCodeWorktree,
  useRefreshCodeWorktree,
  useSetProjectCodeWorktree,
} from "@multica/core/projects";
import { api } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { Button } from "@multica/ui/components/ui/button";
import {
  isDesktopShell,
  pickDirectory,
  inspectLocalCodeWorktree,
  useLocalDaemonStatus,
} from "../../platform";
import { useT } from "../../i18n";

export function ProjectCodeWorktreeSection({
  projectId,
  defaultCodeWorktreeId,
}: {
  projectId: string;
  defaultCodeWorktreeId: string | null;
}) {
  const wsId = useWorkspaceId();
  const { t } = useT("projects");
  const daemon = useLocalDaemonStatus();
  const desktop = isDesktopShell();
  const [busy, setBusy] = useState(false);
  const { data: worktrees = [] } = useQuery(codeWorktreesOptions(wsId));
  const create = useCreateCodeWorktree(wsId);
  const refresh = useRefreshCodeWorktree(wsId);
  const setProjectWorktree = useSetProjectCodeWorktree(wsId, projectId);
  const bound = worktrees.find((item) => item.id === defaultCodeWorktreeId);

  const inspect = async (localPath: string, worktreeId?: string) => {
    if (!daemon.running || !daemon.daemonId) {
      throw new Error(t(($) => $.worktree.daemon_required));
    }
    const inspection = await api.createCodeWorktreeInspection({
      daemon_id: daemon.daemonId,
      local_path: localPath,
      ...(worktreeId ? { worktree_id: worktreeId } : {}),
    });
    if (!inspection.id) throw new Error(t(($) => $.worktree.inspect_create_failed));
    const local = await inspectLocalCodeWorktree(inspection.id, localPath);
    if (!local.ok) throw new Error(local.error);
    const completed = await api.getCodeWorktreeInspection(inspection.id);
    if (completed.status !== "completed") {
      throw new Error(completed.error || t(($) => $.worktree.inspect_incomplete));
    }
    return completed;
  };

  const addAndBind = async () => {
    if (busy) return;
    const picked = await pickDirectory();
    if (!picked.ok || !picked.path) return;
    setBusy(true);
    try {
      const inspection = await inspect(picked.path);
      const worktree = await create.mutateAsync({ inspection_id: inspection.id });
      if (!worktree.id) throw new Error(t(($) => $.worktree.invalid_response));
      await setProjectWorktree.mutateAsync({ worktree_id: worktree.id });
      toast.success(t(($) => $.worktree.toast_bound));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.worktree.toast_bind_failed));
    } finally {
      setBusy(false);
    }
  };

  const refreshBound = async () => {
    if (!bound || busy) return;
    setBusy(true);
    try {
      const inspection = await inspect(bound.local_path, bound.id);
      await refresh.mutateAsync({ id: bound.id, data: { inspection_id: inspection.id } });
      toast.success(t(($) => $.worktree.toast_refreshed));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.worktree.toast_refresh_failed));
    } finally {
      setBusy(false);
    }
  };

  const bindExisting = async (id: string) => {
    try {
      await setProjectWorktree.mutateAsync({ worktree_id: id });
      toast.success(t(($) => $.worktree.toast_bound));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.worktree.toast_bind_failed));
    }
  };

  return (
    <section className="space-y-2 border-t pt-3">
      <div className="flex items-center gap-2 px-2 text-xs font-medium">
        <FolderGit className="size-3.5 text-muted-foreground" />
        <span>{t(($) => $.worktree.section_header)}</span>
      </div>
      {bound ? (
        <div className="rounded-md border bg-muted/30 px-2 py-2 text-xs">
          <p className="truncate font-medium" title={bound.local_path}>{bound.local_path}</p>
          <p className="mt-1 truncate text-muted-foreground">{bound.branch} · {bound.head_sha.slice(0, 8)}</p>
          {bound.is_dirty && <p className="mt-1 text-destructive">{t(($) => $.worktree.dirty_hint)}</p>}
          {desktop && (
            <div className="mt-2 flex gap-1">
              <Button size="xs" variant="outline" disabled={busy} onClick={refreshBound}>
                <RefreshCw className="size-3" /> {t(($) => $.worktree.refresh)}
              </Button>
              <Button size="xs" variant="ghost" disabled={busy} onClick={() => setProjectWorktree.mutate({ worktree_id: null })}>
                <Unlink className="size-3" /> {t(($) => $.worktree.unbind)}
              </Button>
            </div>
          )}
        </div>
      ) : (
        <p className="px-2 text-xs text-muted-foreground">{t(($) => $.worktree.empty)}</p>
      )}
      {desktop && !bound && (
        <div className="space-y-1 px-2">
          <Button size="sm" variant="outline" disabled={busy || !daemon.running} onClick={addAndBind}>
            <FolderGit className="size-3.5" /> {t(($) => $.worktree.choose)}
          </Button>
          {worktrees.filter((item) => item.daemon_id === daemon.daemonId).map((item) => (
            <button key={item.id} type="button" className="block w-full truncate text-left text-xs text-muted-foreground hover:text-foreground" onClick={() => bindExisting(item.id)}>
              {t(($) => $.worktree.use_existing, { path: item.local_path, branch: item.branch })}
            </button>
          ))}
          {!daemon.running && <p className="text-xs text-muted-foreground">{t(($) => $.worktree.daemon_offline_hint)}</p>}
        </div>
      )}
      {!desktop && bound && <p className="px-2 text-xs text-muted-foreground">{t(($) => $.worktree.web_read_only_hint)}</p>}
    </section>
  );
}
