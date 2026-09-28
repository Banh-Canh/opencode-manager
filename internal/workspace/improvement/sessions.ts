// Manager-owned. Runs with the Bun runtime already supplied by the base image.
// SQLite is opened read-only; a read transaction provides consistent snapshots
// even when another workspace is writing its WAL. No OpenCode process is started.
import { Database } from "bun:sqlite";
import { createHash, randomUUID } from "node:crypto";
import * as fs from "node:fs";
import * as path from "node:path";
import { parseArgs } from "node:util";
import { consolidate } from "./memory";

type Options = { since?: string; until?: string; workspace?: string; all?: boolean };
type Paths = { workspaces: string; config: string; output: string };
type Session = {
  key: string; workspace: string; agent: string; id: string; title: string;
  updated: number; fingerprint: string; snapshot: string; finding: string;
  family: string; offset: number; next: number; total: number; snapshotHash: string;
  previousFinding?: string;
};
type Run = {
  id: string; started: string; completed?: string; options: Options;
  sessions: Session[]; coverage: Record<string, unknown>[]; errors: string[];
  protocol: number; sequence: number; baseline: string; settingsHash: string; instructionsHash: string;
};
type State = { version: number; sessions: Record<string, { fingerprint: string; run: string; started: string; sequence?: number; offset?: number }> };
type Settings = {
  analysis: { initialDays: number; maxSessionsPerWorkspace: number; maxCharsPerRun: number; maxCharsPerSession: number; maxWorkers: number };
  roots: { name: string; path: string; readOnly?: boolean; description?: string }[];
};

function settingsAt(paths: Paths): Settings {
  const file = path.join(paths.output, "settings.json");
  const settings: Partial<Settings> = fs.existsSync(file) ? readJSON(file) : {};
  const analysis = { initialDays: 7, maxSessionsPerWorkspace: 20, maxCharsPerRun: 240000, maxCharsPerSession: 60000, maxWorkers: 4, ...settings.analysis };
  for (const [key, value] of Object.entries(analysis)) {
    if (!Number.isSafeInteger(value) || (value as number) < 1) throw new Error(`Invalid analysis limit: ${key}`);
  }
  return { analysis, roots: settings.roots || [{ name: "manager-config", path: paths.config, readOnly: false }] };
}

function baselineAt(output: string, days: number) {
  fs.mkdirSync(output, { recursive: true, mode: 0o700 });
  const file = path.join(output, "baseline.json");
  try {
    fs.writeFileSync(file, JSON.stringify({ since: new Date(Date.now() - days * 86400000).toISOString() }), { flag: "wx", mode: 0o600 });
  } catch (error: any) { if (error.code !== "EEXIST") throw error; }
  return readJSON(file).since as string;
}

function hash(value: string | Buffer) {
  return createHash("sha256").update(value).digest("hex");
}
function readJSON(file: string) { return JSON.parse(fs.readFileSync(file, "utf8")); }
function atomicJSON(file: string, value: unknown) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const temp = `${file}.${randomUUID()}.tmp`;
  try {
    fs.writeFileSync(temp, JSON.stringify(value, null, 2) + "\n", { mode: 0o600 });
    fs.renameSync(temp, file);
  } finally {
    fs.rmSync(temp, { force: true });
  }
}
function stateAt(output: string): State {
  const file = path.join(output, "state.json");
  if (!fs.existsSync(file)) return { version: 1, sessions: {} };
  const state = readJSON(file);
  if (state.version !== 1 || !state.sessions || typeof state.sessions !== "object") {
    throw new Error("Unsupported or invalid state.json; refusing to discard prior progress");
  }
  return state;
}
function date(value: string | undefined, fallback: number): number {
  if (value === undefined) return fallback;
  // Explicit UTC/offset for timestamps avoids container-local timezone surprises.
  if (!/^\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}(?::\d{2}(?:\.\d{1,3})?)?(?:Z|[+-]\d{2}:\d{2}))?$/.test(value)) {
    throw new Error(`Invalid date ${value}; use YYYY-MM-DD or an ISO timestamp with timezone`);
  }
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) throw new Error(`Invalid date: ${value}`);
  return timestamp;
}
function jsonFiles(directory: string): string[] {
  if (!fs.existsSync(directory)) return [];
  return fs.readdirSync(directory, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name)).flatMap(entry => {
    const file = path.join(directory, entry.name);
    return entry.isDirectory() ? jsonFiles(file) : entry.isFile() && entry.name.endsWith(".json") ? [file] : [];
  });
}

// Only paths and digests are retained, not copies of configuration secrets.
function harnessIndex(root: string) {
  const files: Record<string, string> = {};
  function walk(dir: string) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      if (["node_modules", ".git"].includes(entry.name)) continue;
      const file = path.join(dir, entry.name);
      if (entry.isSymbolicLink()) files[path.relative(root, file)] = `symlink:${fs.readlinkSync(file)}`;
      else if (entry.isDirectory()) walk(file);
      else if (entry.isFile()) files[path.relative(root, file)] = hash(fs.readFileSync(file));
    }
  }
  walk(root);
  return { root, fingerprint: hash(JSON.stringify(files)), files };
}

function preparing<T>(output: string, fn: () => T): T {
  fs.mkdirSync(output, { recursive: true, mode: 0o700 });
  const lock = path.join(output, ".preparation-lock");
  fs.mkdirSync(lock, { mode: 0o700 });
  try { return fn(); } finally { fs.rmdirSync(lock); }
}

export function prepare(paths: Paths, options: Options = {}): Run {
  return preparing(paths.output, () => prepareRun(paths, options));
}

function prepareRun(paths: Paths, options: Options): Run {
  const settings = settingsAt(paths);
  const baseline = baselineAt(paths.output, settings.analysis.initialDays);
  const since = date(options.since, options.all || options.until ? -Infinity : date(baseline, -Infinity));
  const until = date(options.until, Infinity);
  if (since >= until) throw new Error("--since must be earlier than --until");
  const all = options.all || options.since !== undefined || options.until !== undefined;
  const state = stateAt(paths.output);
  const entries = fs.readdirSync(paths.workspaces, { withFileTypes: true })
    .filter(e => e.isDirectory() && fs.existsSync(path.join(paths.workspaces, e.name, "workspace.yaml")))
    .sort((a, b) => a.name.localeCompare(b.name));
  if (options.workspace && !entries.some(e => e.name === options.workspace)) {
    throw new Error(`Workspace slug not found: ${options.workspace}`);
  }
  const sequenceFile = path.join(paths.output, "sequence.json");
  const sequence = (fs.existsSync(sequenceFile) ? readJSON(sequenceFile).sequence : 0) + 1;
  if (!Number.isSafeInteger(sequence) || sequence < 1) throw new Error("Invalid preparation sequence");
  atomicJSON(sequenceFile, { sequence });
  const id = `${new Date().toISOString().replace(/[:.]/g, "-")}-${randomUUID().slice(0, 8)}`;
  const dir = path.join(paths.output, "runs", id);
  fs.mkdirSync(path.join(dir, "sessions"), { recursive: true, mode: 0o700 });
  fs.mkdirSync(path.join(dir, "findings"), { mode: 0o700 });
  fs.mkdirSync(path.join(paths.output, "proposals"), { recursive: true, mode: 0o700 });
  const instructionsFile = path.join(paths.output, "AGENTS.md");
  const instructions = fs.existsSync(instructionsFile) ? fs.readFileSync(instructionsFile, "utf8") : "";
  const run: Run = { id, started: new Date().toISOString(), options, sessions: [], coverage: [], errors: [],
    protocol: 2, sequence, baseline, settingsHash: hash(JSON.stringify(settings)), instructionsHash: hash(instructions) };
  fs.writeFileSync(path.join(dir, "AGENTS.md"), instructions, { mode: 0o600 });
  atomicJSON(path.join(dir, "settings.json"), settings);
  const candidates: { session: Omit<Session, "offset" | "next" | "total" | "snapshotHash">; content: string }[] = [];
  try {
    atomicJSON(path.join(dir, "harness.json"), { roots: settings.roots.map(root => ({ ...root, ...harnessIndex(root.path) })) });
  } catch (error) {
    run.errors.push(`Harness inventory: ${String(error)}`);
  }
  for (const entry of entries) {
    const workspace = entry.name;
    if (options.workspace && options.workspace !== workspace) continue;
    const home = path.join(paths.workspaces, workspace, "home");
    const data = path.join(home, ".local/share/opencode");
    const coverage = { workspace, agent: "opencode", store: "none", total: 0, eligible: 0, selected: 0, pending: 0, partial: 0, characters: 0, outsideWindow: 0, unsupportedAgents: [] as string[] };
    run.coverage.push(coverage);
    if (fs.existsSync(path.join(home, ".claude/projects"))) coverage.unsupportedAgents.push("claude");
    if (fs.existsSync(path.join(home, ".config/deepseek/sessions"))) coverage.unsupportedAgents.push("deepseek");
    const parents = new Map<string, string>();
    const accept = (info: any, records: unknown[], updated: number) => {
      coverage.total++;
      parents.set(info.id, info.parent_id || info.parentID || "");
      if (!Number.isFinite(updated)) throw new Error(`Invalid modification time for ${info.id}`);
      const key = JSON.stringify([workspace, "opencode", info.id]);
      if ((updated < since && (all || !state.sessions[key])) || updated >= until) { coverage.outsideWindow++; return; }
      const content = records.map(record => JSON.stringify(record)).join("\n") + "\n";
      const fingerprint = hash(content);
      if (!all && state.sessions[key]?.fingerprint === fingerprint && !state.sessions[key]?.offset) return;
      const token = hash(key);
      const snapshot = `sessions/${token}.jsonl`;
      candidates.push({ session: { key, workspace, agent: "opencode", id: info.id, title: info.title || "",
        updated, fingerprint, snapshot, finding: `findings/${token}.md`, family: info.id }, content });
      coverage.eligible++;
    };
    try {
      const dbFile = path.join(data, "opencode.db");
      if (fs.existsSync(dbFile)) {
        coverage.store = "sqlite";
        const db = new Database(dbFile, { readonly: true });
        try {
          db.exec("PRAGMA busy_timeout = 5000");
          db.transaction(() => {
            const tables = new Set(db.query("SELECT name FROM sqlite_master WHERE type = 'table'").all().map((r: any) => r.name));
            if (!tables.has("session") || (!tables.has("message") && !tables.has("session_message"))) {
              throw new Error("Unsupported OpenCode SQLite schema");
            }
            const statements = ["message", "part", "session_message", "session_input"]
              .filter(table => tables.has(table))
              .map(table => ({ table, query: db.query(`SELECT * FROM ${table} WHERE session_id = ? ORDER BY time_created, id`) }));
            for (const session of db.query("SELECT * FROM session ORDER BY id").all() as any[]) {
              const records: unknown[] = [{ type: "session", ...session }];
              let updated = session.time_updated;
              for (const { table, query } of statements) {
                for (const row of query.all(session.id) as any[]) {
                  updated = Math.max(updated, row.time_updated || row.time_created || 0);
                  records.push({ table, ...row, ...(typeof row.data === "string" ? { data: JSON.parse(row.data) } : {}) });
                }
              }
              accept(session, records, updated);
            }
          })();
        } finally { db.close(); }
      } else {
        // Pre-SQLite OpenCode stores one JSON document per session/message/part.
        const storage = path.join(data, "storage");
        const sessions = jsonFiles(path.join(storage, "session"));
        coverage.store = sessions.length ? "legacy-json" : "none";
        for (const file of sessions) {
          const info = readJSON(file);
          if (!/^ses_[\w-]+$/.test(info.id)) throw new Error(`Invalid session ID in ${file}`);
          const records: unknown[] = [{ type: "session", ...info }];
          let updated = info.time?.updated;
          for (const messageFile of jsonFiles(path.join(storage, "message", info.id))) {
            const message = readJSON(messageFile);
            if (!/^msg_[\w-]+$/.test(message.id)) throw new Error(`Invalid message ID in ${messageFile}`);
            records.push({ table: "message", ...message });
            updated = Math.max(updated, message.time?.completed || message.time?.created || 0);
            for (const partFile of jsonFiles(path.join(storage, "part", message.id))) {
              records.push({ table: "part", ...readJSON(partFile) });
              updated = Math.max(updated, fs.statSync(partFile).mtimeMs);
            }
          }
          accept(info, records, updated);
        }
      }
    } catch (error) {
      run.errors.push(`${workspace}: ${String(error)}`);
    }
    for (const candidate of candidates.filter(c => c.session.workspace === workspace)) {
      const seen = new Set<string>();
      let family = candidate.session.id;
      while (parents.get(family) && !seen.has(family)) { seen.add(family); family = parents.get(family)!; }
      candidate.session.family = JSON.stringify([workspace, "opencode", family]);
    }
  }
  // Round-robin workspaces so a busy project cannot consume the entire budget.
  const queues = new Map<string, typeof candidates>();
  for (const candidate of candidates.sort((a, b) => a.session.updated - b.session.updated || a.session.key.localeCompare(b.session.key))) {
    const queue = queues.get(candidate.session.workspace) || [];
    queue.push(candidate); queues.set(candidate.session.workspace, queue);
  }
  let remaining = settings.analysis.maxCharsPerRun;
  const active = [...queues.values()];
  const sliceLimit = Math.min(settings.analysis.maxCharsPerSession, Math.max(1, Math.floor(remaining / Math.max(1, active.length))));
  for (let round = 0; round < settings.analysis.maxSessionsPerWorkspace && remaining > 0; round++) {
    for (const queue of active) {
      const candidate = queue.shift();
      if (!candidate || remaining <= 0) continue;
      const { session, content } = candidate;
      const previous = state.sessions[session.key];
      const offset = previous?.fingerprint === session.fingerprint ? previous.offset || 0 : 0;
      const next = Math.min(content.length, offset + sliceLimit, offset + remaining);
      const chunk = content.slice(offset, next);
      const selected: Session = { ...session, offset, next, total: content.length, snapshotHash: hash(chunk),
        ...(previous ? { previousFinding: `runs/${previous.run}/${session.finding}` } : {}) };
      fs.writeFileSync(path.join(dir, session.snapshot), chunk, { mode: 0o600 });
      run.sessions.push(selected);
      remaining -= chunk.length;
      const coverage = run.coverage.find(c => c.workspace === session.workspace)! as any;
      coverage.selected++; coverage.characters += chunk.length;
      if (next < content.length) coverage.partial++;
    }
  }
  for (const coverage of run.coverage as any[]) coverage.pending = coverage.eligible - coverage.selected + coverage.partial;
  atomicJSON(path.join(dir, "run.json"), run);
  return run;
}

function runDir(output: string, id: string) {
  if (!/^[\w-]+$/.test(id)) throw new Error("Invalid run ID");
  return path.join(output, "runs", id);
}
function nonempty(file: string) {
  if (!fs.readFileSync(file, "utf8").trim()) throw new Error(`Empty required result: ${file}`);
}

export function complete(output: string, id: string) {
  const dir = runDir(output, id);
  const lock = path.join(output, ".checkpoint-lock");
  // Atomic directory creation also prevents two manager processes from losing
  // each other's checkpoint updates. No lock is held during model analysis.
  fs.mkdirSync(lock, { mode: 0o700 });
  try {
    const run: Run = readJSON(path.join(dir, "run.json"));
    if (run.completed) return run;
    if (run.errors.length) throw new Error("Run has coverage errors; resolve them and prepare a new run before checkpointing");
    nonempty(path.join(dir, "report.md"));
    for (const session of run.sessions) {
      nonempty(path.join(dir, session.finding));
      if (hash(fs.readFileSync(path.join(dir, session.snapshot))) !== (session.snapshotHash || session.fingerprint)) {
        throw new Error(`Snapshot was modified: ${session.snapshot}`);
      }
    }
    if (run.protocol === 2) consolidate(output, run);
    const state = stateAt(output);
    for (const session of run.sessions) {
      const previous = state.sessions[session.key];
      // Completing an older pending run must not replace a newer checkpoint.
      if (!previous || (previous.sequence !== undefined && run.sequence !== undefined ? previous.sequence <= run.sequence : previous.started <= run.started)) {
        state.sessions[session.key] = { fingerprint: session.fingerprint, run: id, started: run.started,
          ...(run.sequence !== undefined ? { sequence: run.sequence } : {}),
          ...(session.next < session.total ? { offset: session.next } : {}) };
      }
    }
    atomicJSON(path.join(output, "state.json"), state);
    run.completed = new Date().toISOString();
    atomicJSON(path.join(dir, "run.json"), run);
    return run;
  } finally { fs.rmdirSync(lock); }
}

export function status(output: string) {
  const root = path.join(output, "runs");
  if (!fs.existsSync(root)) return [];
  return fs.readdirSync(root).sort().map(id => {
    const file = path.join(root, id, "run.json");
    if (!fs.existsSync(file)) return { id, incompletePreparation: true };
    const run: Run = readJSON(file);
    return { id, started: run.started, sequence: run.sequence, completed: run.completed || null,
      sessions: run.sessions.length, options: run.options, errors: run.errors, coverage: run.coverage };
  });
}

export function start(paths: Paths, options: Options = {}): Run {
  return preparing(paths.output, () => resumeOrPrepare(paths, options));
}

function resumeOrPrepare(paths: Paths, options: Options): Run {
  const settingsHash = hash(JSON.stringify(settingsAt(paths)));
  const file = path.join(paths.output, "AGENTS.md");
  const instructionsHash = hash(fs.existsSync(file) ? fs.readFileSync(file, "utf8") : "");
  for (const item of status(paths.output).sort((a, b) => (b.sequence || 0) - (a.sequence || 0))) {
    if (item.completed || item.incompletePreparation || item.errors?.length) continue;
    const run: Run = readJSON(path.join(runDir(paths.output, item.id), "run.json"));
    if (run.protocol === 2 && run.settingsHash === settingsHash && run.instructionsHash === instructionsHash &&
      selectionKey(run.options) === selectionKey(options)) return run;
  }
  return prepareRun(paths, options);
}

function selectionKey(options: Options) {
  return JSON.stringify([options.since || null, options.until || null, options.workspace || null, !!options.all]);
}

if (import.meta.main) {
  try {
    const { positionals, values } = parseArgs({ args: process.argv.slice(2), allowPositionals: true, strict: true,
      options: { since: { type: "string" }, until: { type: "string" }, workspace: { type: "string" },
        all: { type: "boolean" }, offset: { type: "string" }, limit: { type: "string" } } });
    const paths: Paths = { workspaces: "/mnt/workspaces", config: "/mnt/manager-config", output: import.meta.dir };
    const [command, id, token] = positionals;
    if (["prepare", "start"].includes(command) && positionals.length === 1 && !values.offset && !values.limit) {
      const run = command === "start" ? start(paths, values) : prepare(paths, values);
      console.log(JSON.stringify({ id: run.id, directory: runDir(paths.output, run.id), sessions: run.sessions.length,
        coverage: run.coverage, errors: run.errors }, null, 2));
      if (run.errors.length) process.exitCode = 1;
    } else if (command === "complete" && positionals.length === 2 && !Object.keys(values).length) {
      const run = complete(paths.output, id);
      console.log(JSON.stringify({ id, completed: run.completed, sessions: run.sessions.length }));
    } else if (command === "status" && positionals.length === 1 && !Object.keys(values).length) {
      console.log(JSON.stringify(status(paths.output), null, 2));
    } else if (command === "read" && positionals.length === 3 && /^[a-f0-9]{64}$/.test(token)) {
      const offset = Number(values.offset || 0), limit = Number(values.limit || 12000);
      if (!Number.isInteger(offset) || offset < 0 || !Number.isInteger(limit) || limit < 1 || limit > 24000) {
        throw new Error("read requires a nonnegative offset and a limit between 1 and 24000 characters");
      }
      const content = fs.readFileSync(path.join(runDir(paths.output, id), "sessions", `${token}.jsonl`), "utf8");
      console.log(JSON.stringify({ offset, next: Math.min(offset + limit, content.length), total: content.length,
        content: content.slice(offset, offset + limit) }));
    } else {
      throw new Error("Usage: bun sessions.ts start|prepare [--since DATE] [--until DATE] [--workspace SLUG] [--all] | complete RUN | status | read RUN TOKEN [--offset N] [--limit N]");
    }
  } catch (error) {
    console.error(String(error));
    process.exitCode = 1;
  }
}
