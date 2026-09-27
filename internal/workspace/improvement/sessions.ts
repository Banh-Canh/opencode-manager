// Manager-owned. Runs with the Bun runtime already supplied by the base image.
// SQLite is opened read-only; a read transaction provides consistent snapshots
// even when another workspace is writing its WAL. No OpenCode process is started.
import { Database } from "bun:sqlite";
import { createHash, randomUUID } from "node:crypto";
import * as fs from "node:fs";
import * as path from "node:path";
import { parseArgs } from "node:util";

type Options = { since?: string; until?: string; workspace?: string; all?: boolean };
type Paths = { workspaces: string; config: string; output: string };
type Session = {
  key: string; workspace: string; agent: string; id: string; title: string;
  updated: number; fingerprint: string; snapshot: string; finding: string;
};
type Run = {
  id: string; started: string; completed?: string; options: Options;
  sessions: Session[]; coverage: Record<string, unknown>[]; errors: string[];
};
type State = { version: number; sessions: Record<string, { fingerprint: string; run: string; started: string }> };

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

export function prepare(paths: Paths, options: Options = {}): Run {
  const since = date(options.since, -Infinity);
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
  const id = `${new Date().toISOString().replace(/[:.]/g, "-")}-${randomUUID().slice(0, 8)}`;
  const dir = path.join(paths.output, "runs", id);
  fs.mkdirSync(path.join(dir, "sessions"), { recursive: true, mode: 0o700 });
  fs.mkdirSync(path.join(dir, "findings"), { mode: 0o700 });
  fs.mkdirSync(path.join(paths.output, "proposals"), { recursive: true, mode: 0o700 });
  const run: Run = { id, started: new Date().toISOString(), options, sessions: [], coverage: [], errors: [] };
  try {
    atomicJSON(path.join(dir, "harness.json"), harnessIndex(paths.config));
  } catch (error) {
    run.errors.push(`Harness inventory: ${String(error)}`);
  }
  for (const entry of entries) {
    const workspace = entry.name;
    if (options.workspace && options.workspace !== workspace) continue;
    const home = path.join(paths.workspaces, workspace, "home");
    const data = path.join(home, ".local/share/opencode");
    const coverage = { workspace, store: "none", total: 0, selected: 0, unsupportedAgents: [] as string[] };
    run.coverage.push(coverage);
    if (fs.existsSync(path.join(home, ".claude/projects"))) coverage.unsupportedAgents.push("claude");
    if (fs.existsSync(path.join(home, ".config/deepseek/sessions"))) coverage.unsupportedAgents.push("deepseek");
    const accept = (info: any, records: unknown[], updated: number) => {
      coverage.total++;
      if (!Number.isFinite(updated)) throw new Error(`Invalid modification time for ${info.id}`);
      if (updated < since || updated >= until) return;
      const key = JSON.stringify([workspace, "opencode", info.id]);
      const content = records.map(record => JSON.stringify(record)).join("\n") + "\n";
      const fingerprint = hash(content);
      if (!all && state.sessions[key]?.fingerprint === fingerprint) return;
      const token = hash(key);
      const snapshot = `sessions/${token}.jsonl`;
      fs.writeFileSync(path.join(dir, snapshot), content, { mode: 0o600 });
      run.sessions.push({ key, workspace, agent: "opencode", id: info.id, title: info.title || "",
        updated, fingerprint, snapshot, finding: `findings/${token}.md` });
      coverage.selected++;
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
  }
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
      if (hash(fs.readFileSync(path.join(dir, session.snapshot))) !== session.fingerprint) {
        throw new Error(`Snapshot was modified: ${session.snapshot}`);
      }
    }
    const state = stateAt(output);
    for (const session of run.sessions) {
      const previous = state.sessions[session.key];
      // Completing an older pending run must not replace a newer checkpoint.
      if (!previous || previous.started <= run.started) {
        state.sessions[session.key] = { fingerprint: session.fingerprint, run: id, started: run.started };
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
    return { id, started: run.started, completed: run.completed || null,
      sessions: run.sessions.length, options: run.options, errors: run.errors };
  });
}

if (import.meta.main) {
  try {
    const { positionals, values } = parseArgs({ args: process.argv.slice(2), allowPositionals: true, strict: true,
      options: { since: { type: "string" }, until: { type: "string" }, workspace: { type: "string" },
        all: { type: "boolean" }, offset: { type: "string" }, limit: { type: "string" } } });
    const paths: Paths = { workspaces: "/mnt/workspaces", config: "/mnt/manager-config", output: import.meta.dir };
    const [command, id, token] = positionals;
    if (command === "prepare" && positionals.length === 1 && !values.offset && !values.limit) {
      const run = prepare(paths, values);
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
      throw new Error("Usage: bun sessions.ts prepare [--since DATE] [--until DATE] [--workspace SLUG] [--all] | complete RUN | status | read RUN TOKEN [--offset N] [--limit N]");
    }
  } catch (error) {
    console.error(String(error));
    process.exitCode = 1;
  }
}
