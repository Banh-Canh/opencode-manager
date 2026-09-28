// Manager-owned cumulative evidence and proposal ledger. No model context is
// needed to count recurrences, enforce deduplication, or preserve decisions.
import * as fs from "node:fs";
import * as path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import { parseArgs } from "node:util";

type Evidence = { session: string; reference: string; kind: "occurrence" | "counterexample" };
type ObservationInput = {
  id: string; title: string; summary: string; impact: "low" | "medium" | "high";
  confidence: "low" | "medium" | "high"; tags: string[]; files: string[]; evidence: Evidence[];
};
type EvidenceRecord = Evidence & { run: string; family: string; workspace: string; fingerprint: string; offset: number; observedAt: string; sessionUpdated: number; harness: string };
type Observation = Omit<ObservationInput, "evidence"> & {
  firstSeen: string; lastSeen: string; status: string; evidence: Record<string, EvidenceRecord>;
  history: { at: string; status: string; reason: string }[];
};
type Change = { root: string; path: string; beforeHash: string | null; content: string | null };
type ProposalInput = { id: string; title: string; rationale: string; observationIds: string[]; verification: string; changes: Change[] };
type Proposal = ProposalInput & {
  run: string; status: string; created: string;
  history: { at: string; status: string; reason: string }[];
};
type Memory = { version: 1; observations: Record<string, Observation>; proposals: Record<string, Proposal>; analyzedFamilies: Record<string, string> };
const digest = (data: string | Buffer) => createHash("sha256").update(data).digest("hex");
const read = (file: string) => JSON.parse(fs.readFileSync(file, "utf8"));
const identifier = (id: unknown): id is string => typeof id === "string" && /^[a-z0-9][a-z0-9-]{0,95}$/.test(id) && !Object.hasOwn(Object.prototype, id);
const text = (value: unknown): value is string => typeof value === "string" && !!value.trim();

function load(output: string): Memory {
  const file = path.join(output, "memory.json");
  if (!fs.existsSync(file)) return { version: 1, observations: {}, proposals: {}, analyzedFamilies: {} };
  const memory = read(file);
  if (memory.version !== 1 || !memory.observations || !memory.proposals || !memory.analyzedFamilies) throw new Error("Invalid memory.json");
  return memory;
}
function save(output: string, memory: Memory) {
  const file = path.join(output, "memory.json"), temp = `${file}.${randomUUID()}.tmp`;
  try {
    fs.writeFileSync(temp, JSON.stringify(memory, null, 2) + "\n", { mode: 0o600 });
    fs.renameSync(temp, file);
  } finally { fs.rmSync(temp, { force: true }); }
}
function locked<T>(output: string, fn: () => T): T {
  const lock = path.join(output, ".checkpoint-lock");
  fs.mkdirSync(lock, { mode: 0o700 });
  try { return fn(); } finally { fs.rmdirSync(lock); }
}

// Called by complete while holding the checkpoint lock. Replaying a run after an
// interrupted completion is idempotent; versions and child sessions never inflate
// the displayed count of distinct conversation families.
export function consolidate(output: string, run: any) {
  const dir = path.join(output, "runs", run.id);
  const inputs: ObservationInput[] = read(path.join(dir, "observations.json"));
  if (!Array.isArray(inputs)) throw new Error("observations.json must be an array (including [] for no observations)");
  const memory = load(output);
  const harness = digest(fs.readFileSync(path.join(dir, "harness.json")));
  for (const input of inputs) {
    if (!identifier(input.id) || !text(input.title) || !text(input.summary) ||
      !["low", "medium", "high"].includes(input.impact) || !["low", "medium", "high"].includes(input.confidence) ||
      !Array.isArray(input.tags) || !input.tags.every(text) || !Array.isArray(input.files) || !input.files.every(text) ||
      !Array.isArray(input.evidence) || !input.evidence.length) throw new Error(`Invalid observation: ${input.id}`);
    const existing = memory.observations[input.id];
    const observation: Observation = existing || { ...input, firstSeen: run.started, lastSeen: run.started,
      status: "watching", evidence: {}, history: [] };
    if (run.started >= observation.lastSeen) {
      Object.assign(observation, { title: input.title, summary: input.summary, impact: input.impact, confidence: input.confidence,
        tags: [...new Set([...observation.tags, ...input.tags])], files: [...new Set([...observation.files, ...input.files])], lastSeen: run.started });
    }
    if (run.started < observation.firstSeen) observation.firstSeen = run.started;
    for (const evidence of input.evidence) {
      const session = run.sessions.find((s: any) => s.key === evidence.session);
      if (!session || !text(evidence.reference) || !["occurrence", "counterexample"].includes(evidence.kind)) {
        throw new Error(`Observation ${input.id} must reference a selected session and concrete evidence`);
      }
      const key = digest(JSON.stringify([session.key, session.fingerprint, session.offset || 0, evidence.kind, evidence.reference]));
      observation.evidence[key] ||= { ...evidence, run: run.id, family: session.family || session.key,
        workspace: session.workspace, fingerprint: session.fingerprint, offset: session.offset || 0, observedAt: run.started,
        sessionUpdated: session.updated, harness };
    }
    memory.observations[input.id] = observation;
  }
  for (const session of run.sessions) memory.analyzedFamilies[session.family || session.key] = session.workspace;
  const proposalsFile = path.join(dir, "proposals.json");
  const proposals: ProposalInput[] = fs.existsSync(proposalsFile) ? read(proposalsFile) : [];
  if (!Array.isArray(proposals)) throw new Error("proposals.json must be an array");
  for (const input of proposals) {
    if (!identifier(input.id) || !text(input.title) || !text(input.rationale) || !text(input.verification) ||
      !Array.isArray(input.observationIds) || !input.observationIds.length || !input.observationIds.every(id => identifier(id) && !!memory.observations[id]) ||
      !Array.isArray(input.changes) || !input.changes.length) throw new Error(`Invalid proposal: ${input.id}`);
    const paths = new Set<string>();
    for (const change of input.changes) {
      if (!identifier(change.root) || !text(change.path) || path.isAbsolute(change.path) || change.path.split(/[\\/]/).includes("..") ||
        (change.beforeHash !== null && !/^[a-f0-9]{64}$/.test(change.beforeHash)) ||
        (change.content !== null && typeof change.content !== "string")) throw new Error(`Invalid change in proposal ${input.id}`);
      const key = `${change.root}/${path.normalize(change.path)}`;
      if (paths.has(key)) throw new Error(`Duplicate change: ${key}`);
      paths.add(key);
      const inventory = read(path.join(dir, "harness.json")).roots.find((r: any) => r.name === change.root);
      if (!inventory || inventory.readOnly || (inventory.files[change.path] ?? null) !== change.beforeHash) {
        throw new Error(`Proposal ${input.id}: target is read-only, unknown, or differs from the harness snapshot`);
      }
    }
    const previous = memory.proposals[input.id];
    if (previous) {
      for (const key of ["title", "rationale", "observationIds", "verification", "changes"] as const) {
        if (JSON.stringify(previous[key]) !== JSON.stringify(input[key])) throw new Error(`Proposal ${input.id} already exists; use a new revision ID`);
      }
    } else memory.proposals[input.id] = { ...input, run: run.id, status: "proposed", created: run.started, history: [] };
  }
  save(output, memory);
}

export function query(output: string, options: { query?: string; workspace?: string; status?: string; limit?: number; offset?: number } = {}) {
  const memory = load(output);
  const observations = Object.values(memory.observations).map(observation => {
    const evidence = Object.values(observation.evidence).filter(e => !options.workspace || e.workspace === options.workspace);
    const occurrences = new Set(evidence.filter(e => e.kind === "occurrence").map(e => e.family)).size;
    const counterexamples = new Set(evidence.filter(e => e.kind === "counterexample").map(e => e.family)).size;
    const { evidence: _, ...summary } = observation;
    return { ...summary, occurrences, counterexamples, workspaces: [...new Set(evidence.map(e => e.workspace))] };
  }).filter(o => (!options.status || o.status === options.status) && (!options.workspace || o.workspaces.length > 0) &&
    (!options.query || JSON.stringify(o).toLowerCase().includes(options.query.toLowerCase())))
    .sort((a, b) => b.occurrences - a.occurrences || b.lastSeen.localeCompare(a.lastSeen));
  const proposals = Object.values(memory.proposals).filter(p => (!options.status || p.status === options.status) &&
    (!options.workspace || p.observationIds.some(id => observations.some(o => o.id === id))) &&
    (!options.query || JSON.stringify({ ...p, changes: undefined }).toLowerCase().includes(options.query.toLowerCase())))
    .map(({ changes, ...proposal }) => ({ ...proposal, files: changes.map(c => `${c.root}/${c.path}`) }));
  const offset = options.offset || 0, limit = Math.min(options.limit || 20, 100);
  return { analyzedFamilies: Object.values(memory.analyzedFamilies).filter(w => !options.workspace || w === options.workspace).length,
    // This is coverage, not a denominator of relevant opportunities for each issue.
    totalObservations: observations.length, totalProposals: proposals.length,
    observations: observations.slice(offset, offset + limit), proposals: proposals.slice(offset, offset + limit) };
}

export function show(output: string, id: string, offset = 0, limit = 20) {
  if (!identifier(id)) throw new Error("Invalid observation/proposal ID");
  const memory = load(output);
  if (memory.proposals[id]) return memory.proposals[id];
  const observation = memory.observations[id];
  if (!observation) throw new Error(`Unknown observation/proposal: ${id}`);
  const { evidence, ...summary } = observation;
  const values = Object.values(evidence).sort((a, b) => b.observedAt.localeCompare(a.observedAt));
  return { ...summary, totalEvidence: values.length, evidence: values.slice(offset, offset + limit) };
}

export function decide(output: string, id: string, status: string, reason: string) {
  if (!identifier(id)) throw new Error("Invalid proposal ID");
  if (!text(reason)) throw new Error("Record the user's reason or instruction");
  return locked(output, () => {
    const memory = load(output), proposal = memory.proposals[id];
    if (!proposal) throw new Error(`Unknown proposal: ${id}`);
    const allowed: Record<string, string[]> = {
      proposed: ["accepted", "rejected", "deferred"], deferred: ["accepted", "rejected", "proposed"],
      rejected: ["proposed"], accepted: ["deferred", "rejected"], applied: ["evaluating", "closed"], evaluating: ["closed"], closed: ["evaluating"],
    };
    if (!allowed[proposal.status]?.includes(status)) throw new Error(`Cannot change ${proposal.status} to ${status}`);
    proposal.status = status; proposal.history.push({ at: new Date().toISOString(), status, reason });
    save(output, memory); return proposal;
  });
}

export function observe(output: string, id: string, status: string, reason: string) {
  if (!identifier(id) || !["watching", "actionable", "resolved", "dismissed"].includes(status) || !text(reason)) {
    throw new Error("Observation requires a valid ID, watching/actionable/resolved/dismissed status and a reason");
  }
  return locked(output, () => {
    const memory = load(output), observation = memory.observations[id];
    if (!observation) throw new Error(`Unknown observation: ${id}`);
    observation.status = status;
    observation.history.push({ at: new Date().toISOString(), status, reason });
    save(output, memory);
    return { id, status };
  });
}

function targetPath(root: string, relative: string) {
  const base = fs.realpathSync(root), target = path.resolve(base, relative);
  if (!target.startsWith(base + path.sep)) throw new Error("Target must be a file within its configured root");
  // Reject symlinks at every level, including an existing leaf.
  let cursor = base;
  for (const part of path.relative(base, target).split(path.sep)) {
    cursor = path.join(cursor, part);
    if (fs.existsSync(cursor) && fs.lstatSync(cursor).isSymbolicLink()) throw new Error(`Symlink target: ${cursor}`);
    try { if (fs.lstatSync(cursor).isSymbolicLink()) throw new Error(`Symlink target: ${cursor}`); }
    catch (error: any) { if (error.code !== "ENOENT") throw error; }
  }
  return target;
}

export function apply(output: string, id: string) {
  if (!identifier(id)) throw new Error("Invalid proposal ID");
  return locked(output, () => {
    const memory = load(output), proposal = memory.proposals[id];
    if (!proposal || proposal.status !== "accepted") throw new Error("Only an accepted proposal can be applied");
    const settings = read(path.join(output, "settings.json"));
    const targets = new Set<string>();
    const changes = proposal.changes.map(change => {
      const root = settings.roots.find((r: any) => r.name === change.root);
      if (!root || root.readOnly) throw new Error(`Target ${change.root} is unavailable or read-only`);
      const file = targetPath(root.path, change.path);
      if (targets.has(file)) throw new Error(`Overlapping proposal targets: ${file}`);
      targets.add(file);
      const before = fs.existsSync(file) ? fs.readFileSync(file) : null;
      if ((before === null ? null : digest(before)) !== change.beforeHash) throw new Error(`Target changed since analysis: ${change.root}/${change.path}`);
      return { change, file, before, mode: before === null ? 0o600 : fs.statSync(file).mode };
    });
    const backup = path.join(output, "applications", `${id}-${randomUUID()}`);
    fs.mkdirSync(backup, { recursive: true, mode: 0o700 });
    fs.writeFileSync(path.join(backup, "before.json"), JSON.stringify(changes.map(c => ({ file: c.file, mode: c.mode,
      content: c.before?.toString("base64") ?? null })), null, 2), { mode: 0o600 });
    const written: typeof changes = [];
    try {
      for (const item of changes) {
        written.push(item);
        if (item.change.content === null) fs.rmSync(item.file, { force: true });
        else {
          fs.mkdirSync(path.dirname(item.file), { recursive: true, mode: 0o700 });
          fs.writeFileSync(item.file, item.change.content, { mode: item.mode });
        }
      }
      proposal.status = "applied";
      proposal.history.push({ at: new Date().toISOString(), status: "applied", reason: `Applied; backup: ${backup}` });
      save(output, memory);
    } catch (error) {
      for (const item of written.reverse()) {
        if (item.before === null) fs.rmSync(item.file, { force: true });
        else fs.writeFileSync(item.file, item.before, { mode: item.mode });
      }
      throw error;
    }
    return { id, status: proposal.status, backup };
  });
}

if (import.meta.main) {
  try {
    const { positionals: args, values } = parseArgs({ args: process.argv.slice(2), allowPositionals: true,
      options: { query: { type: "string" }, workspace: { type: "string" }, status: { type: "string" }, limit: { type: "string" }, offset: { type: "string" } } });
    const output = import.meta.dir;
    const limit = Number(values.limit || 20), offset = Number(values.offset || 0);
    if (!Number.isInteger(limit) || limit < 1 || limit > 100 || !Number.isInteger(offset) || offset < 0) throw new Error("Invalid pagination");
    let result: unknown;
    if (args[0] === "query" && args.length === 1) result = query(output, { ...values, limit, offset });
    else if (args[0] === "show" && args.length === 2) result = show(output, args[1], offset, limit);
    else if (args[0] === "decision" && args.length === 4) result = decide(output, args[1], args[2], args[3]);
    else if (args[0] === "observation" && args.length === 4) result = observe(output, args[1], args[2], args[3]);
    else if (args[0] === "apply" && args.length === 2) result = apply(output, args[1]);
    else throw new Error("Usage: bun memory.ts query [--query TEXT] [--workspace SLUG] [--status STATUS] [--offset N] [--limit N] | show ID | decision ID STATUS REASON | observation ID STATUS REASON | apply ID");
    console.log(JSON.stringify(result, null, 2));
  } catch (error) { console.error(String(error)); process.exitCode = 1; }
}
