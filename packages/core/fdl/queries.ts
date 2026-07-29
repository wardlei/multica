import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

// These are projections and mutable templates owned by the server. The FDL
// run root remains the source of truth for Controller state and evidence.
export const fdlKeys = {
  all: (wsId: string) => ["fdl", wsId] as const,
  profiles: (wsId: string) => [...fdlKeys.all(wsId), "profiles"] as const,
  profile: (wsId: string, profileId: string) =>
    [...fdlKeys.profiles(wsId), profileId] as const,
  issueRun: (wsId: string, issueId: string) =>
    [...fdlKeys.all(wsId), "issue-run", issueId] as const,
};

export function fdlDeliveryProfilesOptions(wsId: string) {
  return queryOptions({
    queryKey: fdlKeys.profiles(wsId),
    queryFn: () => api.listFDLDeliveryProfiles(),
  });
}

export function fdlIssueRunOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: fdlKeys.issueRun(wsId, issueId),
    queryFn: () => api.getFDLIssueRun(issueId),
    enabled: !!issueId,
  });
}
