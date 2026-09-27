import { afterEach, expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { prepare, complete, status } from "./improvement/sessions";

const cleanups: (() => void)[] = [];
afterEach(() => { while (cleanups.length) cleanups.pop()!(); });
function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "ocm-analysis-"));
  cleanups.push(() => fs.rmSync(root, { recursive: true, force: true }));
  const paths = { workspaces: path.join(root, "workspaces"), config: path.join(root, "config"), output: path.join(root, "internal") };
  for (const dir of Object.values(paths)) fs.mkdirSync(dir);
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
  f.results(newer); complete(f.paths.output, newer.id);
  f.results(older); complete(f.paths.output, older.id);
  expect(prepare(f.paths).sessions).toHaveLength(0);
});
