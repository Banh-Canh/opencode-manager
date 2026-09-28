# Persisted analysis format

OCM owns helpers, default agents and commands. Customize the host
`self-improvement/AGENTS.md`, not generated workspace files.

## Findings

Write one nonempty Markdown finding at each `session.finding` in run.json.
Include the session key, conversation family, analyzed offsets, concrete
message/part IDs or snapshot character ranges, observations, counterexamples,
confidence, uncertainty and suggested scope. Minor issues belong here even when
no immediate proposal is justified. Never put credentials in artifacts.

## observations.json

A JSON array, including `[]` when nothing was found. Existing IDs are discovered
through `bun memory.ts query --query TEXT` and `show ID`. Stable lowercase kebab
IDs identify issues, not runs. The helper counts distinct conversation families,
not evidence rows or session versions. Sources/workspaces are part of session keys.

```json
[
  {
    "id": "knowledge-entry-point",
    "title": "Repeated questions about the knowledge base location",
    "summary": "Low individual cost, repeated user interruptions; location may be insufficiently explicit.",
    "impact": "low",
    "confidence": "medium",
    "tags": ["knowledge", "interruptions"],
    "files": ["manager-config/opencode/AGENTS.md"],
    "evidence": [
      {
        "session": "<exact session.key string from run.json>",
        "reference": "message msg_123, part prt_456; snapshot characters 1200–1600",
        "kind": "occurrence"
      }
    ]
  }
]
```

impact/confidence: low, medium, high. kind: occurrence or counterexample.
An occurrence can document a beneficial practice; explain this in the summary.
The merger adds run, fingerprint, chunk offset, workspace, family, analysis time
and harness inventory hash. Evidence remains tied to immutable run artifacts.
Queries return bounded summaries; `show ID --offset N --limit N` pages evidence.
The global analyzed-family count measures coverage, NOT relevant opportunities
for a particular issue. Do not invent a rate from it.

After completion, the memory analyst can set an observation's lifecycle with
`bun memory.ts observation ID STATUS "evidence-based reason"`: watching,
actionable, resolved or dismissed. Reopen a resolved issue only with new evidence.
Proposal accept/reject/defer decisions always belong to the user.

## proposals.json

An optional JSON array (`[]` when none). Several independent proposals are expected
when justified. Each must refer to existing/new observation IDs. Configuration-only
findings without session evidence may be reported as hypotheses, not fabricated
recurrences. The normal proposals focus on evidenced improvements.

```json
[
  {
    "id": "knowledge-entry-point-v1",
    "title": "Document the shared knowledge base entry point",
    "rationale": "Repeated navigation questions in several distinct conversations.",
    "observationIds": ["knowledge-entry-point"],
    "verification": "Look for navigation corrections in future relevant sessions; separate pre/post application.",
    "changes": [
      {
        "root": "manager-config",
        "path": "opencode/AGENTS.md",
        "beforeHash": "<SHA-256 from the root's files inventory in harness.json>",
        "content": "<complete proposed UTF-8 file contents>"
      }
    ]
  }
]
```

Use beforeHash null for a new file; content null deletes an existing file.
Paths are relative to a named writable root. Symlink targets are not supported.
Display a readable diff in report.md and in the discussion; full contents in the
artifact make application independent of git. Existing IDs are immutable, including
after rejection. Use a new revision ID and explain what changed.

## Decisions and application

`bun memory.ts decision ID accepted "user requested application"`
`bun memory.ts apply ID`

Other decisions: rejected, deferred; deferred/rejected can return to proposed with
a recorded reason. Applied changes can become evaluating or closed. The applied
status is written only by the application helper. Failed applications restore
files on ordinary errors; interruption recovery uses applications/*/before.json
(base64 original contents, null for an originally absent file). No git operations
are required or performed. Do not modify memory.json/state.json manually.
