"use client";

import { useQuery } from "@tanstack/react-query";
import { Workflow } from "lucide-react";
import { useWorkspaceId } from "@multica/core/hooks";
import { fdlIssueRunOptions } from "@multica/core/fdl";
import { useT } from "../../i18n";

// The card intentionally renders the server's non-authoritative projection.
// Controller state, evidence, and recovery decisions stay in the FDL run root.
export function FDLDeliveryStatus({ issueId }: { issueId: string }) {
  const wsId = useWorkspaceId();
  const { t } = useT("issues");
  const { data: run, isError } = useQuery(fdlIssueRunOptions(wsId, issueId));

  if (isError) {
    return (
      <section className="rounded-md border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-xs">
        <div className="flex items-center gap-1.5 font-medium">
          <Workflow className="size-3.5" />
          {t(($) => $.fdl_delivery.title)}
        </div>
        <p className="mt-1 text-muted-foreground">{t(($) => $.fdl_delivery.unavailable)}</p>
      </section>
    );
  }

  if (!run) return null;
  const waitingReason = typeof run.state_projection.waiting_reason === "string"
    ? run.state_projection.waiting_reason
    : null;

  return (
    <section className="rounded-md border bg-muted/20 px-3 py-2.5 text-xs">
      <div className="flex items-center gap-1.5 font-medium">
        <Workflow className="size-3.5 text-primary" />
        {t(($) => $.fdl_delivery.title)}
      </div>
      <p className="mt-1 text-muted-foreground">{t(($) => $.fdl_delivery.controller_authority)}</p>
      <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-2 gap-y-1">
        <dt className="text-muted-foreground">{t(($) => $.fdl_delivery.status)}</dt>
        <dd className="truncate font-medium">{run.status}</dd>
        <dt className="text-muted-foreground">{t(($) => $.fdl_delivery.phase)}</dt>
        <dd className="truncate font-medium">{run.phase}</dd>
        {waitingReason && (
          <>
            <dt className="text-muted-foreground">{t(($) => $.fdl_delivery.waiting_reason)}</dt>
            <dd className="truncate" title={waitingReason}>{waitingReason}</dd>
          </>
        )}
      </dl>
    </section>
  );
}
