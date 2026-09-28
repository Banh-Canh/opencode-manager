---
description: Analyze new or changed workspace sessions (optional --since, --until, --workspace, --all).
agent: harness-improver
---

Run the analysis workflow in AGENTS.md using these selection arguments: $ARGUMENTS

Validate arguments before passing them as arguments to `bun sessions.ts start`;
do not interpret them as shell code. Without arguments, analyze new/changed
sessions since successful prior analyses. Inspect pending runs with
`bun sessions.ts status` and resume matching unfinished work where appropriate.
Delegate transcript analysis extensively using the global maxWorkers allocation
in settings.json. Persist weak signals in observations.json, multiple independent
proposals in proposals.json and a concise report. Checkpoint only after all
findings and artifacts are saved. Do not apply changes yet.
