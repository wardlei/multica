# Workspace Worktree Context Implementation Review

## Scope

This review reconciles the Phase 1 design with migrations, SQL queries, server
handlers, task snapshots, daemon preflight, desktop bridge, shared API schemas,
CLI commands, and built-in CLI documentation.

The automated OCR client was available and its model connection passed. It was
not run in workspace mode because this checkout contains pre-existing,
user-owned daemon edits that cannot be separated by OCR's range interface. The
review therefore used a focused cross-artifact inspection of the worktree
feature files and the affected existing task/daemon paths.

## Findings

No unresolved P1 or P2 findings remain in the reviewed implementation scope.

The review corrected these implementation defects before this re-review:

- A Project could have gained a legacy `local_directory` after binding a Code
  Worktree on the same daemon. Both resource creation and resource update now
  reject that ambiguous execution context.
- Deferred issue fallback tasks omitted `worktree_context`. They now resolve
  and store the same immutable snapshot as direct, mention, leader, rerun, and
  retry task paths.
- A malformed worktree-create response could have reached the bind action with
  an empty ID. The desktop flow now refuses that response before it can be
  interpreted as an unbind.
- Project binding now locks the Project and target worktree in one transaction
  and emits the normal `project:updated` event after commit.
- Task inserts and automatic retries take a `KEY SHARE` guard on the referenced
  Code Worktree. Deletion takes the conflicting worktree lock, then rechecks
  active snapshots, so a concurrently created snapshot cannot outlive a
  successful deletion.
- Legacy `local_directory` create/update operations now lock the Project in the
  same transaction as the conflict check and resource write. A concurrent
  Code Worktree bind on the same daemon therefore resolves to one valid
  execution context, never both.

The immutable task-context invariant was re-keyed after deferred-task coverage
was added:

- object: `agent-task-worktree-snapshot`
- inputs: `agent_task_queue.worktree_context;CreateAgentTask;CreateDeferredAgentTask;RetryAgentTask;daemon preflight`
- invariant key: `02e950c1c6021e916168e1c25ad56945597f86c7a4c5d003ce96ce44e66d48ea`

## Coverage

| Object | Result | Evidence |
| --- | --- | --- |
| Inspection proof | covered | Expiring, daemon-bound inspection rows; daemon-authenticated completion; one-time consumption; refresh CAS. |
| Project binding | covered | Workspace ownership checks, Project/worktree row locks, transactionally serialized bidirectional legacy-directory conflict checks, and delete guards. |
| Immutable task context | covered | Direct, mention, squad-leader, deferred fallback, manual rerun, and retry paths store or copy `worktree_context`; task insertion/retry and worktree deletion are mutually guarded. |
| Daemon preflight | covered | Canonical-path, daemon, normalized origin, branch, SHA, and clean-state checks run before local execution. |
| Client contract | covered | Zod plus `parseWithFallback` schemas, React Query invalidation, desktop local-daemon IPC, four-locale UI copy, and CLI inspection/add/list/bind commands. |
| Focused regression tests | covered | Handler tests cover same-daemon legacy-directory rejection and active-task deletion blocking; daemon, schema, and shared-view tests cover preflight, malformed responses, and web read-only display. |
| Migration contract | covered | No foreign keys; concurrent indexes live in standalone migration files; local migration was applied during development. |

## Verification

Passed after the final changes:

```text
go test ./internal/daemon ./internal/handler ./internal/service ./cmd/server ./cmd/multica
pnpm typecheck --filter @multica/core --filter @multica/views --filter @multica/desktop
pnpm test --filter @multica/core --filter @multica/views
git diff --check
```

The TypeScript suite completed with 2,975 `@multica/views` tests passing. Its
existing jsdom/React warnings did not fail the command.

## Residual Risks

Phase 1 deliberately works in a user-selected existing checkout. It does not
create or clean worktrees, switch branches, create branches, commit, push, or
open pull requests. Those operations require a separate isolated-worktree
lifecycle and recovery design.
