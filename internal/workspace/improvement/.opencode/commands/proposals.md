---
description: Review prior improvement proposals and record the user's decisions.
agent: harness-improver
---

Use bounded `bun memory.ts query` and `show ID` calls, then list proposals with stable ID,
scope, evidence, target files and status. User request: $ARGUMENTS

Use `bun memory.ts decision ID STATUS "reason"` to record explicit decisions. Apply only
proposals the user asks you to apply; first re-read their target files and verify
the proposal still matches the current configuration using `bun memory.ts apply ID`.
Scope includes OCM configuration, harnesses and knowledge bases in writable roots.
Preserve unrelated edits and explain required restarts. For legacy V1 proposals,
read individual files in proposals/ on request; never infer a new acceptance.
