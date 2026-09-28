# Harness self improvement

Run **`ocm improve`** or press **`i`** on the dashboard to start an incremental
analysis immediately. A dedicated OpenCode agent examines recent workspace
sessions, delegates analysis, maintains cumulative observations, and proposes
several independent improvements. You can discuss, defer, reject or apply each
proposal in the same conversation. No slash command is required to start.

## Enable and configure

In `~/.config/opencode-manager/config.yaml` (or the platform-specific directory
printed by `ocm config path`):

```yaml
selfImprovement:
  enabled: true
  instructions:
    mode: extend
  analysis:
    initialDays: 7
    maxSessionsPerWorkspace: 20
    maxCharsPerRun: 240000
    maxCharsPerSession: 60000
    maxWorkers: 4
  directories:
    - name: knowledge
      path: ~/my-knowledge-base
      description: Shared procedures and project conventions
      readOnly: false
    - name: reference
      path: /absolute/path/to/reference
      description: Reference documentation, not an improvement target
      readOnly: true
```

Only `enabled: true` is required; omit `directories` when none are needed. All
analysis values shown above are defaults. Zero uses the default; negative values
are invalid. Optional `agent: opencode` is accepted; other analysis runtimes are
not supported yet. Restart OCM after editing its configuration.

The default target is the **entire OCM configuration directory**, not just its
`opencode/` subdirectory: OCM settings, harness configuration, AGENTS.md, skills,
commands, knowledge bases and their organization can all be improved. Changes to
shared OpenCode configuration still belong in the source `opencode/` directory,
which OCM synchronizes one-way into ordinary workspaces.

Additional directories use unique lowercase kebab-case names, an absolute host
path or `~/` path, an optional description, and `readOnly` (default false). They
must exist when starting the analysis container. They are mounted only into the
private instance at `/mnt/improvement/<name>`. Read-only roots can inform an
analysis but cannot be proposal targets. Git is optional for every root.

## Personal instructions

Edit this file, automatically created with a commented skeleton:

```text
~/.config/opencode-manager/self-improvement/AGENTS.md
```

For example:

```markdown
# My self-improvement priorities

- Prioritize interruptions that require me to correct the agent.
- Keep minor recurring frictions on watch even without an immediate proposal.
- Prefer simplifying existing instructions over adding new ones.

## Context
- The knowledge root contains shared procedures, not project source code.
- Do not generalize preferences specific to one project.
```

You can also ask the agent to add a preference to this file. Changes take effect
on the next `ocm improve`/`i` launch; exit the current OpenCode session and reopen
it to reload instructions and agent definitions.

- **extend** (default): OCM's built-in protocol plus your file. Personal analysis
  preferences take precedence. OCM updates its defaults without replacing your
  customization.
- **replace**: your nonempty personal instructions replace the built-in AGENTS.md
  protocol. Technical helpers, artifact validation, budgets and checkpoints remain
  manager-owned. `PROTOCOL.md` in the private workspace documents their contract.

OCM composes the effective project AGENTS.md. Each analysis snapshots it, and all
subagents are instructed to read that frozen copy. Other AGENTS.md files in the
audited roots are evidence, not instructions for the analysis agent.

## First analysis, budgets and incremental progress

The first preparation fixes a baseline **seven days before that preparation** by
default. Earlier history is outside the initial scope, not marked as analyzed.
Later launches do not automatically work backwards through that old history.
`initialDays` changes the first baseline only; use explicit date filters to examine
other history after initialization.

The helper limits both session count per workspace and characters selected per
run/session. Workspaces are selected round-robin, oldest pending work first.
Very large sessions are divided into windows across runs; findings from earlier
windows provide context. Coverage records selected characters, partial sessions,
pending sessions and sessions outside the window. The character budget bounds
selected transcript content, not total model tokens or the metadata/database scan.

Subsequent runs select new or modified content, including changed parts of older
previously analyzed sessions. Progress identifies source, workspace, session,
content fingerprint and, for a partial version, the next offset. Changed partial
versions restart at offset zero rather than mixing incompatible versions.
Completed session versions are not reanalyzed unless explicitly requested.

`start` reuses an unfinished run when selection arguments, settings and effective
instructions match. Snapshots and saved findings survive interruption. A new
launch has a fresh orchestration context; it does not accumulate the entire prior
conversation. Preparation and completion are serialized separately. An unreadable
workspace is reported as a coverage error and prevents completion, rather than
being silently treated as empty. Successful findings remain available for inspection.

Progress is checkpointed after findings, observations and a report are persisted,
**independently of whether you accept any proposal**. Older pending runs cannot
replace a newer checkpoint. Modifying saved transcript snapshots is detected.

## Delegation and cumulative memory

The default protocol uses:

- a harness/configuration analyst;
- workspace coordinators, which delegate bounded session windows;
- session analysts for raw transcript reading;
- a memory analyst to reconcile findings with existing observations and decisions;
- a primary orchestrator that receives compact summaries and prioritizes proposals.

`maxWorkers` is the requested total active subagent budget across the hierarchy.
With small budgets, roles run sequentially and the orchestrator delegates session
reading directly. This concurrency policy is implemented in the prompts, not a
hard runtime scheduler. Reading budgets and ledger validation are enforced by code.

The durable ledger distinguishes **observations** from **proposals**. A small issue
can remain on watch for several analyses, gain evidence and later justify a change.
It retains evidence, counterexamples, confidence, affected files, workspaces and
run references. Multiple session versions or parent/child sessions do not inflate
the count of distinct conversation families. Bounded queries retrieve relevant
summaries without loading the entire ledger into a model's context.

Proposals retain decisions and reasons: proposed, accepted, deferred, rejected,
applied, evaluating, closed. Observation statuses are watching, actionable,
resolved and dismissed. Rejected ideas should return only with an explanation of
new evidence or changed circumstances. Applied changes can be evaluated against
later sessions; inventory hashes describe the harness at analysis time and do
not prove which configuration an old session used. Analytical matching of related
issues and interpretation of pre/post-change evidence remain agent responsibilities.

The expected result is a concise coverage summary and normally three to five
independent recommendations, each with examples, target files, a readable diff,
expected benefit and a verification criterion. There is no quota: no justified
change is a valid result. You can ask about observations on watch, why an issue
has become important, or whether a previous change helped.

## Applying proposals

The agent records your explicit acceptance before applying a proposal. The helper
checks target hashes against the analysis snapshot, rejects read-only/unknown roots
and symlink targets, backs up original files and records successful application.
It refuses to overwrite files modified since the analysis. Independent proposals
that change the same file may require rebasing after the first is applied.

This works without Git and does not initialize repositories or create commits.
Backups live under `applications/`. Ordinary write failures restore original
contents; after a process interruption, the corresponding `before.json` contains
base64 original contents (null means an originally absent file) for recovery.
The agent explains which OCM/OpenCode instances need restarting afterward.

## Commands and storage

Inside the dedicated OpenCode conversation, optional commands remain available:

```text
/analyze
/analyze --since 2026-09-01 --until 2026-09-15
/analyze --workspace my-project
/analyze --all
/audit-harness
/proposals
```

Workspace filters use directory slugs. Dates filter modification times, including
message/part updates; since is inclusive and until is exclusive. Date-only values
mean midnight UTC; timestamps need a timezone. Date filters and `--all` explicitly
reexamine matching content but still respect reading budgets. `/audit-harness`
does not advance session progress.

The internal instance lives outside ordinary workspace lists and selectors:

```text
<workspaceRoot>/internal/self-improvement/
  workspace.yaml
  home/workspace/
    AGENTS.md                    # generated effective instructions
    PROTOCOL.md                  # artifact schemas and helper contract
    opencode.json
    .opencode/agents/            # manager-owned analysis roles
    .opencode/commands/
    settings.json                # limits and named container roots
    sessions.ts
    memory.ts
    baseline.json
    sequence.json
    state.json                   # incremental progress
    memory.json                  # cumulative observations and decisions
    runs/<run-id>/
      run.json
      AGENTS.md                  # frozen effective instructions
      settings.json
      harness.json               # named-root file/hash inventories
      sessions/
      findings/
      observations.json
      proposals.json
      report.md
    applications/                # original contents before applying proposals
    legacy-instructions/         # preserved earlier/customized generated files
    proposals/                   # retained V1 proposals, if any
```

The container mounts the manager configuration at `/mnt/manager-config` read-write
and ordinary workspace homes at `/mnt/workspaces` read-only. Optional additional
roots use `/mnt/improvement/<name>`. Overlapping container targets from global
extra mounts are rejected. Changing private mounts participates in container drift
detection. The instance inherits the base image, environment, authentication,
certificates, networking and container runtime; project post-create hooks do not run.

Helpers for diagnostics (normally driven by the agent):

```sh
bun sessions.ts start
bun sessions.ts status
bun sessions.ts prepare --workspace my-project
bun sessions.ts read RUN TOKEN --offset 0 --limit 12000
bun sessions.ts complete RUN
bun memory.ts query --query knowledge --limit 20
bun memory.ts query --workspace my-project --status watching
bun memory.ts show OBSERVATION-OR-PROPOSAL-ID
bun memory.ts decision PROPOSAL-ID deferred "User wants more evidence"
```

If a process is killed while holding `.preparation-lock` or `.checkpoint-lock`,
verify no corresponding helper is running before removing the stale lock directory.
Do not edit progress or memory files manually. Disabling the feature preserves data.

## Session sources and upgrades

The reader supports OpenCode SQLite databases (read-only transactions, including
active WAL) and legacy JSON stores under each ordinary workspace's
`home/.local/share/opencode/`. Containers need not be running. Its own history and
directories without workspace manifests are excluded. Custom session-store paths
are not scanned. DSH and Claude histories are not analyzed yet; detected history
directories are reported as unsupported. Source identity is included in session
keys so future readers can maintain separate progress.

On upgrade, existing progress and reports are retained. A customized V1 internal
AGENTS.md is backed up and copied into personal instructions once; the obsolete
stock protocol is replaced. Earlier agent/command files are archived before
installing managed versions; local opencode.json preferences are preserved.
Review those archives to transfer any role-specific
customization into the personal file. V1 proposal files remain available for review
but are not automatically interpreted as new acceptances or evidence. Older reports
are not silently converted into cumulative observations; explicit reanalysis can
populate the new ledger.
