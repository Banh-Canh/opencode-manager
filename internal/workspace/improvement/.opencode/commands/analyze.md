---
description: Analyze new or changed workspace sessions (optional --since, --until, --workspace, --all).
agent: harness-improver
---

Run the analysis workflow in AGENTS.md using these selection arguments: $ARGUMENTS

Validate arguments before passing them as arguments to `bun sessions.ts prepare`;
do not interpret them as shell code. Without arguments, analyze new/changed
sessions since successful prior analyses. Inspect pending runs with
`bun sessions.ts status` and resume matching unfinished work where appropriate.
Delegate transcript analysis extensively, at most four workers at a time. Produce
a report and concrete proposed configuration/instruction diffs, then checkpoint
only when all findings and the report are saved. Do not apply changes yet.
