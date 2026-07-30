# Review Loop Configuration

This directory is the version-controlled declaration for the local Multica
`Review Loop` Squad. It captures the workspace state that determines review
quality without committing user-specific IDs, runtime bindings, credentials, or
database files.

It configures an existing `Reviewer` agent and `Review Loop` Squad to:

- bind the tracked `open-code-review` workspace skill to Reviewer;
- require evidence-backed review findings and an explicit verdict;
- require a fresh review after each implementation change; and
- stop after five completed reviews with unresolved findings surfaced as
  blocked.

## Apply

Authenticate the Multica CLI for the target server, then run:

```bash
config/review-loop/apply.sh \
  --server-url http://localhost:18080 \
  --workspace-id <workspace-uuid>
```

The script is idempotent. It resolves the target agent and squad by name, so it
fails rather than silently modifying the wrong resource when either name is
missing or duplicated. It does not create agents, squads, runtimes, or
credentials.
