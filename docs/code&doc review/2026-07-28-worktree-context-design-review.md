# Workspace Worktree Context Design Review

## Findings

### Resolved P1: Immutable Task Context Had No Stored Contract

The first design revision said that a task carried an immutable worktree
snapshot, but did not specify where it was stored or how retry, rerun, refresh,
unbind, and deletion interacted with it. Resolving the Project at claim time
would let a queued task silently execute in a different checkout; allowing
deletion based only on Project pointers would leave live tasks referencing a
removed binding.

The revision stores the snapshot in `agent_task_queue.worktree_context` within
the enqueue transaction, makes automatic retry inherit it and manual rerun
resolve a fresh one, and blocks deletion while a non-terminal task references
the worktree. The changed design section also requires enqueue to lock and
validate the worktree, closing the refresh/delete race.

- invariant inputs: `agent-task-worktree-snapshot`, `immutable-context`, and
  `docs/2026-07-28-worktree-context-design.md;agent_task_queue;retry/rerun semantics`
- invariant key: `1b3d0fea925d656fffe11ee8de7da0fed5657cb6e5419e729ee467e2efceca80`

## Re-review Result

No unresolved P1-P3 findings remain in the revised Phase 1 design contract.
This is a design-consistency review, not evidence that the implementation or
its tests already satisfy the contract.

## Coverage

The machine-readable matrix is in `2026-07-28-worktree-context-coverage.json`.
It covers the inspection proof, mutable Project binding, and immutable task
snapshot across authority, lifecycle, concurrency, permissions, and
observability. The review checked the design against the current
`project_resource`, task-claim, and daemon local-directory paths.

## Residual Risks

The design intentionally defers isolated worktree and branch lifecycle. Phase
1 runs only in a user-selected existing checkout and must retain the existing
path-lock cancellation behavior when implemented.
