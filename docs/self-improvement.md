# Harness self improvement

The self-improvement workspace uses OpenCode to analyze historical sessions and
propose changes to configurations and instructions. It is an internal instance:
it does not appear in workspace lists, selectors, completion candidates, or bulk
workspace operations.

## Enable and open

Add this to `~/.config/opencode-manager/config.yaml` (or the platform-specific
path printed by `ocm config path`):

```yaml
selfImprovement:
  enabled: true
```

Restart the manager. The persistent layout is created automatically without
starting an analysis. Open it with:

```sh
ocm improve
```

Alternatively, press **`i`** on the workspace dashboard, including when the list
is empty. Only OpenCode is available for this internal instance. The optional
`selfImprovement.agent: opencode` setting makes that explicit.

The instance inherits the base image, shared OpenCode configuration, workspace
environment, authentication setting, certificates, extra mounts, networking and
container runtime. Project-specific post-create commands are not run in it.
Provider authentication works the same way as in ordinary workspaces.
The session helper uses Bun, included in current managed base images.

## Analyze

Inside the dedicated OpenCode session:

```text
/analyze
/analyze --since 2026-09-01 --until 2026-09-15
/analyze --workspace my-project
/analyze --all
/audit-harness
/proposals
```

- `/analyze` selects new or changed session content since successful analyses.
- `--workspace` accepts a workspace **directory slug**.
- Dates filter by session modification time, including message/part updates.
  `--since` is inclusive, `--until` is exclusive. Date-only values mean midnight
  UTC; timestamps require a timezone. An explicit date range re-examines all
  matching sessions, even if previously analyzed.
- `--all` re-examines all matching sessions.
- `/audit-harness` examines current configurations and instructions without
  advancing session progress.
- `/proposals` reviews proposed changes and records user decisions.

The coordinator delegates transcript reading and configuration auditing to
dedicated subagents, with instructions to run at most four workers concurrently.
It receives compact findings rather than loading every conversation into its
context. Long snapshots can be read in bounded character windows. This
delegation policy is defined in the prompts, not enforced by a scheduler.

Each proposal should include evidence, confidence, appropriate scope, target
files, a concrete diff, and a criterion for checking its benefit in later
sessions. The agent proposes changes first and applies them on request. Its
scope is configurations and instructions, including prompts, commands and skills;
manager/application code and executable plugins or module hooks are out of scope.

## Access and storage

The internal instance lives at:

```text
<workspaceRoot>/internal/self-improvement/
  workspace.yaml
  home/workspace/
    AGENTS.md
    opencode.json
    .opencode/agents/
    .opencode/commands/
    sessions.ts
    state.json
    runs/<run-id>/
      run.json
      harness.json
      sessions/
      findings/
      report.md
    proposals/
```

Its container has two additional **read-write** mounts:

| Host location | Container location |
|---|---|
| Manager configuration directory | `/mnt/manager-config` |
| `<workspaceRoot>/workspaces` | `/mnt/workspaces` |

Mount targets overlapping these paths are rejected. Ordinary workspaces do not
receive these mounts. Shared global improvements belong in
`/mnt/manager-config/opencode`, since managed workspace configuration copies are
synchronized one way from that source.

The dedicated `AGENTS.md`, project configuration, agent prompts and slash commands
are seeded once, preserving subsequent customization. `sessions.ts` is a
manager-owned helper updated on reconciliation. Restart the dedicated OpenCode
instance after changing its OpenCode configuration, commands or agent definitions;
running instances retain the configuration they loaded at startup.

## Session coverage and checkpoints

The V1 reader supports OpenCode's SQLite database and legacy JSON session store
under each workspace's `home/.local/share/opencode/`. It includes child/subagent
sessions and reads SQLite in read-only transactions, including active WAL data.
Other containers do not need to be running. The reader excludes its own history
and directories without a workspace manifest, such as preserved deleted homes.
Custom session locations outside the conventional workspace home are not scanned.
Claude/DeepSeek histories are not analyzed in this version; detected history
directories are recorded in the coverage report.

Preparation writes immutable-by-convention transcript snapshots and per-session
content hashes. Reports reference `harness.json`, a path/hash inventory of the
manager configuration at preparation time; it contains no configuration copies.
It is not a reconstruction of the configuration used by historical sessions.

The helper checkpoints progress only after a nonempty report and a finding for
every selected session exist. It verifies snapshot hashes and refuses runs with
coverage errors. Only successfully analyzed versions of selected sessions are
marked: a date/workspace filter cannot skip unrelated history, and later message
changes are selected again. Completing an older run does not overwrite newer
progress. These checks verify persisted artifacts, not their analytical quality.

To inspect or resume an interrupted analysis, use `/analyze` and ask to resume the
pending run. Its snapshots and existing findings are retained. The underlying
commands, available from the internal project directory, are:

```sh
bun sessions.ts status
bun sessions.ts prepare --workspace my-project
bun sessions.ts read <run-id> <snapshot-token> --offset 0 --limit 12000
bun sessions.ts complete <run-id>
```

Checkpoint writes are atomic and serialized through `.checkpoint-lock`. If the
helper is killed during completion, verify no completion is still running before
removing that stale lock directory and retrying. Do not edit `state.json` manually.
Disabling the feature retains all reports, proposals and session state.
