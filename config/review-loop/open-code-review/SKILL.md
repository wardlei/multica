---
name: open-code-review
description: Use for implementation or diff reviews that need evidence-backed, line-level findings and an OCR-assisted review pass.
---

# Open Code Review

Use this skill when reviewing a code change, commit, pull request, or branch
comparison. Review the actual repository state; an implementation summary is
not evidence that a change is correct.

## Prerequisites

Before the first OCR-assisted review in a task, confirm that the local tool is
available and can reach its configured model:

```bash
which ocr
ocr llm test
```

If either check fails, do not invent review results. Report the review as
blocked and include the failed check.

## Review Procedure

1. Establish the target: inspect the issue scope, acceptance criteria, and the
   relevant Git range or working-tree diff.
2. Read the changed code and affected tests. Use repository conventions and
   adjacent call sites to evaluate correctness, contracts, security, data
   handling, regressions, and missing coverage.
3. Run OCR with concise business context when a code diff is available:

```bash
ocr review --audience agent --background "<issue scope and acceptance criteria>" [diff-selection-flags]
```

Use an explicit commit or range when one is known. Do not review unrelated
untracked files merely because they happen to be in the working tree.

4. Verify OCR findings against source before reporting them. OCR output is an
   input to review, not proof on its own.

## Required Output

Every review response must contain:

- `Review scope`: the diff, artifact, or commit range inspected.
- `Evidence inspected`: commands, files, tests, or contracts checked.
- `Findings`: P0/P1/P2 findings, each with a precise file and line reference
  when applicable, evidence, impact, and a concrete fix direction.
- `Verdict`: exactly `PASS`, `NEEDS_CHANGES`, or `BLOCKED`.

Use `NEEDS_CHANGES` while any actionable P0/P1 finding remains. Use `PASS`
only after inspecting the latest artifact or diff and finding no unresolved
P0/P1 issue. A generic approval, an empty response, or an unverified
implementation report is never a valid `PASS`.

## Boundaries

Do not modify repository files during a review unless the coordinating agent
explicitly delegates a fix. When a fix is reported, review the resulting diff
again rather than accepting the report at face value.
