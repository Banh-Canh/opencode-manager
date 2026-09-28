import { afterEach, expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { prepare, complete, status, start } from "./improvement/sessions";
import { query, show, decide, apply, observe } from "./improvement/memory";
import { createHash } from "node:crypto";

const cleanups: (() => void)[] = [];
afterEach(() => { while (cleanups.length) cleanups.pop()!(); });
function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "ocm-analysis-"));
  cleanups.push(() => fs.rmSync(root, { recursive: true, force: true }));
  const paths = { workspaces: path.join(root, "workspaces"), config: path.join(root, "config"), output: path.join(root, "internal") };
  for (const dir of Object.values(paths)) fs.mkdirSync(dir);
  fs.writeFileSync(path.join(paths.output, "settings.json"), JSON.stringify({ analysis: { initialDays: 100000 },
    roots: [{ name: "manager-config", path: paths.config }] }));
  fs.writeFileSync(path.join(paths.config, "AGENTS.md"), "Follow project conventions.");
  function store(slug: string) {
    const home = path.join(paths.workspaces, slug, "home");
    const data = path.join(home, ".local/share/opencode");
    fs.mkdirSync(data, { recursive: true });
    fs.writeFileSync(path.join(paths.workspaces, slug, "workspace.yaml"), `name: ${slug}\n`);
    const db = new Database(path.join(data, "opencode.db"));
    cleanups.push(() => db.close());
    db.exec(`PRAGMA journal_mode=WAL;
      CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, title TEXT, time_created INTEGER, time_updated INTEGER);
      CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT);
      CREATE TABLE part (id TEXT PRIMARY KEY, session_id TEXT, message_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT);`);
    return { db, home };
  }
  function session(db: Database, id: string, updated: string, parent: string | null = null) {
    const time = Date.parse(updated);
    db.query("INSERT INTO session VALUES (?, ?, ?, ?, ?)").run(id, parent, `Title ${id}`, time, time);
    db.query("INSERT INTO message VALUES (?, ?, ?, ?, ?)").run(`msg_${id}`, id, time, time, JSON.stringify({ role: "user" }));
    db.query("INSERT INTO part VALUES (?, ?, ?, ?, ?, ?)").run(`prt_${id}`, id, `msg_${id}`, time, time, JSON.stringify({ type: "text", text: "A correction" }));
  }
  function results(run: ReturnType<typeof prepare>) {
    const dir = path.join(paths.output, "runs", run.id);
    fs.writeFileSync(path.join(dir, "report.md"), "Evidence and proposed improvements");
    fs.writeFileSync(path.join(dir, "observations.json"), "[]");
    for (const s of run.sessions) fs.writeFileSync(path.join(dir, s.finding), `Evidence: ${s.id}`);
  }
  return { paths, store, session, results };
}

test("reads live WAL including child sessions; completion checkpoints content, not just timestamps", () => {
  const f = fixture();
  const { db } = f.store("alpha");
  f.session(db, "ses_a", "2026-09-01T10:00:00Z");
  f.session(db, "ses_child", "2026-09-01T11:00:00Z", "ses_a");
  const run = prepare(f.paths);
  expect(run.errors).toEqual([]);
  expect(run.sessions).toHaveLength(2);
  expect(() => complete(f.paths.output, run.id)).toThrow();
  expect(fs.existsSync(path.join(f.paths.output, "state.json"))).toBe(false);
  f.results(run);
  complete(f.paths.output, run.id);
  expect(prepare(f.paths).sessions).toHaveLength(0);
  // Some streaming writes update a part without touching session.time_updated.
  db.query("UPDATE part SET data = ? WHERE id = ?").run(JSON.stringify({ text: "New correction" }), "prt_ses_a");
  expect(prepare(f.paths).sessions.map(s => s.id)).toEqual(["ses_a"]);
  expect(status(f.paths.output).find(r => r.id === run.id)?.completed).toBeTruthy();
});

test("bootstrap excludes old history persistently; budgets retain pending windows and cover workspaces fairly", () => {
  const f = fixture();
  fs.writeFileSync(path.join(f.paths.output, "settings.json"), JSON.stringify({ analysis: {
    initialDays: 7, maxCharsPerRun: 160, maxCharsPerSession: 100, maxSessionsPerWorkspace: 1,
  } }));
  const a = f.store("alpha"), b = f.store("beta");
  f.session(a.db, "ses_old", "2000-01-01T00:00:00Z");
  f.session(a.db, "ses_new", new Date().toISOString());
  f.session(b.db, "ses_new", new Date().toISOString());
  const run = start(f.paths);
  expect(run.sessions).toHaveLength(2);
  expect(run.sessions.every(s => s.id === "ses_new" && s.next === 80 && s.next < s.total)).toBe(true);
  expect(run.coverage.every(c => c.pending === 1)).toBe(true);
  expect(start(f.paths).id).toBe(run.id);
  f.results(run); complete(f.paths.output, run.id);
  const next = start(f.paths);
  expect(next.baseline).toBe(run.baseline);
  expect(next.sessions.every(s => s.offset === 80 && s.next === 160 && !!s.previousFinding)).toBe(true);
  expect(prepare(f.paths, { all: true }).sessions.some(s => s.id === "ses_old")).toBe(true);
  fs.writeFileSync(path.join(f.paths.output, "AGENTS.md"), "New priorities");
  expect(start(f.paths).id).not.toBe(next.id);
  expect(fs.readFileSync(path.join(f.paths.output, "runs", next.id, "AGENTS.md"), "utf8")).toBe("");
});

test("chunk completion eventually checkpoints the full version; modified chunks restart safely", () => {
  const f = fixture(), { db } = f.store("alpha");
  f.session(db, "ses_a", new Date().toISOString());
  fs.writeFileSync(path.join(f.paths.output, "settings.json"), JSON.stringify({ analysis: { maxCharsPerSession: 100 } }));
  let previous = 0;
  for (let i = 0; i < 30; i++) {
    const run = prepare(f.paths);
    if (!run.sessions.length) break;
    expect(run.sessions[0].offset).toBe(previous);
    previous = run.sessions[0].next;
    f.results(run); complete(f.paths.output, run.id);
  }
  expect(prepare(f.paths).sessions).toHaveLength(0);
  db.query("UPDATE part SET data = ?").run(JSON.stringify({ text: "Changed after analysis" }));
  const changed = prepare(f.paths);
  expect(changed.sessions[0].offset).toBe(0);
  expect(changed.sessions[0].previousFinding).toBeTruthy();
});

function observations(f: ReturnType<typeof fixture>, run: ReturnType<typeof prepare>) {
  const input = { id: "repeated-question", title: "Repeated question", summary: "Minor recurring interruption",
    impact: "low", confidence: "high", tags: ["knowledge"], files: ["manager-config/AGENTS.md"],
    evidence: run.sessions.map(s => ({ session: s.key, reference: "message/part correction", kind: "occurrence" })) };
  fs.writeFileSync(path.join(f.paths.output, "runs", run.id, "observations.json"), JSON.stringify([input]));
}

test("cumulative weak signals deduplicate versions and child sessions, retaining counterexamples and decisions", () => {
  const f = fixture(), { db } = f.store("alpha");
  f.session(db, "ses_a", "2026-09-01T00:00:00Z");
  f.session(db, "ses_child", "2026-09-01T00:00:00Z", "ses_a");
  const run = prepare(f.paths); f.results(run); observations(f, run); complete(f.paths.output, run.id);
  expect(query(f.paths.output).observations[0].occurrences).toBe(1);
  complete(f.paths.output, run.id);
  db.query("UPDATE part SET data = ?").run(JSON.stringify({ text: "Same recurring friction" }));
  const changed = prepare(f.paths); f.results(changed); observations(f, changed); complete(f.paths.output, changed.id);
  expect(query(f.paths.output).observations[0].occurrences).toBe(1);
  f.session(db, "ses_new", "2026-09-02T00:00:00Z");
  const newer = prepare(f.paths); f.results(newer); observations(f, newer); complete(f.paths.output, newer.id);
  const result = query(f.paths.output, { query: "knowledge", workspace: "alpha" });
  expect(result.observations[0].occurrences).toBe(2);
  expect(result.analyzedFamilies).toBe(2);
  expect((show(f.paths.output, "repeated-question", 0, 1) as any).evidence).toHaveLength(1);
  const counter = prepare(f.paths, { all: true }); f.results(counter);
  fs.writeFileSync(path.join(f.paths.output, "runs", counter.id, "observations.json"), JSON.stringify([{
    id: "repeated-question", title: "Repeated question", summary: "Sometimes works", impact: "low", confidence: "medium", tags: [], files: [],
    evidence: [{ session: counter.sessions[0].key, reference: "successful discovery", kind: "counterexample" }],
  }]));
  complete(f.paths.output, counter.id);
  expect(query(f.paths.output).observations[0].counterexamples).toBe(1);
  observe(f.paths.output, "repeated-question", "actionable", "Repeated in independent conversations");
  expect(query(f.paths.output, { status: "actionable" }).observations).toHaveLength(1);
});

test("proposals preserve decisions; application uses backups and rejects stale or read-only targets without git", () => {
  const f = fixture(), { db } = f.store("alpha");
  f.session(db, "ses_a", "2026-09-01T00:00:00Z");
  const run = prepare(f.paths); f.results(run); observations(f, run);
  const before = fs.readFileSync(path.join(f.paths.config, "AGENTS.md"), "utf8");
  const proposal = { id: "knowledge-v1", title: "Clarify", rationale: "Repeated interruptions", observationIds: ["repeated-question"],
    verification: "Observe future corrections", changes: [{ root: "manager-config", path: "AGENTS.md",
      beforeHash: createHash("sha256").update(before).digest("hex"), content: "Improved instructions" }] };
  fs.writeFileSync(path.join(f.paths.output, "runs", run.id, "proposals.json"), JSON.stringify([proposal]));
  complete(f.paths.output, run.id);
  expect(() => apply(f.paths.output, proposal.id)).toThrow("accepted");
  decide(f.paths.output, proposal.id, "deferred", "Later");
  expect(query(f.paths.output, { status: "deferred" }).proposals).toHaveLength(1);
  decide(f.paths.output, proposal.id, "accepted", "Apply now");
  fs.writeFileSync(path.join(f.paths.config, "AGENTS.md"), "User edit");
  expect(() => apply(f.paths.output, proposal.id)).toThrow("changed since analysis");
  expect(fs.readFileSync(path.join(f.paths.config, "AGENTS.md"), "utf8")).toBe("User edit");
  fs.writeFileSync(path.join(f.paths.config, "AGENTS.md"), before);
  fs.writeFileSync(path.join(f.paths.output, "settings.json"), JSON.stringify({ roots: [{ name: "manager-config", path: f.paths.config, readOnly: true }] }));
  expect(() => apply(f.paths.output, proposal.id)).toThrow("read-only");
  fs.writeFileSync(path.join(f.paths.output, "settings.json"), JSON.stringify({ roots: [{ name: "manager-config", path: f.paths.config }] }));
  const applied = apply(f.paths.output, proposal.id);
  expect(applied.status).toBe("applied");
  expect(fs.readFileSync(path.join(f.paths.config, "AGENTS.md"), "utf8")).toBe("Improved instructions");
  expect(JSON.parse(fs.readFileSync(path.join(applied.backup, "before.json"), "utf8"))[0].content).toBe(Buffer.from(before).toString("base64"));
  decide(f.paths.output, proposal.id, "evaluating", "Collect post-change evidence");
  expect((show(f.paths.output, proposal.id) as any).history).toHaveLength(4);
});

test("invalid evidence and concurrent ledger updates cannot advance progress", () => {
  const f = fixture(), { db } = f.store("alpha");
  f.session(db, "ses_a", "2026-09-01T00:00:00Z");
  const run = prepare(f.paths); f.results(run); observations(f, run);
  const file = path.join(f.paths.output, "runs", run.id, "observations.json");
  const input = JSON.parse(fs.readFileSync(file, "utf8"));
  input[0].evidence[0].session = "not-selected";
  fs.writeFileSync(file, JSON.stringify(input));
  expect(() => complete(f.paths.output, run.id)).toThrow("selected session");
  expect(fs.existsSync(path.join(f.paths.output, "state.json"))).toBe(false);
  expect(query(f.paths.output).observations).toHaveLength(0);
  observations(f, run);
  fs.mkdirSync(path.join(f.paths.output, ".checkpoint-lock"));
  expect(() => complete(f.paths.output, run.id)).toThrow();
  fs.rmdirSync(path.join(f.paths.output, ".checkpoint-lock"));
  complete(f.paths.output, run.id);
  expect(query(f.paths.output).observations).toHaveLength(1);
});

test("workspace/date filters have half-open boundaries and do not skip unselected history", () => {
  const f = fixture();
  const a = f.store("alpha"), b = f.store("beta");
  f.session(a.db, "ses_a", "2026-09-01T00:00:00Z");
  f.session(a.db, "ses_b", "2026-09-02T00:00:00Z");
  f.session(b.db, "ses_a", "2026-09-01T12:00:00Z");
  const run = prepare(f.paths, { workspace: "alpha", since: "2026-09-01", until: "2026-09-02" });
  expect(run.sessions.map(s => s.id)).toEqual(["ses_a"]);
  f.results(run);
  complete(f.paths.output, run.id);
  expect(prepare(f.paths).sessions).toHaveLength(2);
  expect(prepare(f.paths, { workspace: "alpha", since: "2026-09-01", until: "2026-09-02" }).sessions).toHaveLength(1);
  expect(() => prepare(f.paths, { workspace: "typo" })).toThrow("not found");
  expect(() => prepare(f.paths, { since: "not a date" })).toThrow("Invalid date");
  expect(() => prepare(f.paths, { since: "2026-09-02", until: "2026-09-01" })).toThrow("earlier");
});

test("modified snapshots, missing findings and coverage failures cannot advance progress", () => {
  const f = fixture();
  const { db } = f.store("alpha");
  f.session(db, "ses_a", "2026-09-01T00:00:00Z");
  const run = prepare(f.paths);
  const dir = path.join(f.paths.output, "runs", run.id);
  fs.writeFileSync(path.join(dir, "report.md"), "Report");
  expect(() => complete(f.paths.output, run.id)).toThrow();
  f.results(run);
  fs.appendFileSync(path.join(dir, run.sessions[0].snapshot), "changed");
  expect(() => complete(f.paths.output, run.id)).toThrow("Snapshot was modified");
  const broken = f.store("broken");
  broken.db.exec("DROP TABLE session");
  const next = prepare(f.paths);
  expect(next.errors).toHaveLength(1);
  f.results(next);
  expect(() => complete(f.paths.output, next.id)).toThrow("coverage errors");
  expect(fs.existsSync(path.join(f.paths.output, "state.json"))).toBe(false);
});

test("legacy stores work, preserved deleted workspaces and internal histories are excluded", () => {
  const f = fixture();
  const legacy = path.join(f.paths.workspaces, "legacy");
  const storage = path.join(legacy, "home/.local/share/opencode/storage");
  fs.mkdirSync(path.join(storage, "session/project"), { recursive: true });
  fs.writeFileSync(path.join(legacy, "workspace.yaml"), "name: legacy\n");
  fs.writeFileSync(path.join(storage, "session/project/ses_old.json"), JSON.stringify({ id: "ses_old", time: { updated: 1000 } }));
  fs.mkdirSync(path.join(storage, "message/ses_old"), { recursive: true });
  fs.writeFileSync(path.join(storage, "message/ses_old/msg_old.json"), JSON.stringify({ id: "msg_old", role: "user", time: { created: 1000 } }));
  fs.mkdirSync(path.join(f.paths.workspaces, "deleted"));
  const run = prepare(f.paths);
  expect(run.errors).toEqual([]);
  expect(run.sessions).toHaveLength(1);
  expect(run.coverage).toHaveLength(1);
  expect(run.coverage[0].store).toBe("legacy-json");
  f.results(run);
  complete(f.paths.output, run.id);
  expect(prepare(f.paths).sessions).toHaveLength(0);
});

test("completing an older pending run cannot regress a newer checkpoint", () => {
  const f = fixture();
  const { db } = f.store("alpha");
  f.session(db, "ses_a", "2026-09-01T00:00:00Z");
  const older = prepare(f.paths);
  // Give the pending run an earlier preparation timestamp deterministically.
  older.started = "2026-09-01T00:00:00Z";
  fs.writeFileSync(path.join(f.paths.output, "runs", older.id, "run.json"), JSON.stringify(older));
  db.query("UPDATE part SET data = ?").run(JSON.stringify({ text: "Updated" }));
  const newer = prepare(f.paths);
  // Equal timestamps still have a strict preparation order.
  newer.started = older.started;
  fs.writeFileSync(path.join(f.paths.output, "runs", newer.id, "run.json"), JSON.stringify(newer));
  f.results(newer); complete(f.paths.output, newer.id);
  f.results(older); complete(f.paths.output, older.id);
  expect(prepare(f.paths).sessions).toHaveLength(0);
});
