// One fresh V8 process per artifact/mode/sample. Both guests use this identical host.
import { readFile, writeFile } from "node:fs/promises";
import { performance } from "node:perf_hooks";
import { DatabaseSync } from "node:sqlite";
import { WASI, File, OpenFile, ConsoleStdout } from "../sdk/typescript/node_modules/@bjorn3/browser_wasi_shim/dist/index.js";
import { root } from "./workloads.mjs";

const [artifact, mode, output] = process.argv.slice(2);
if (!artifact || !["guest", "sqlite"].includes(mode) || !output) throw new Error("artifact mode output required");
const bytes = await readFile(artifact);
let start = performance.now();
const module = await WebAssembly.compile(bytes);
const compileMs = performance.now() - start;
const wasi = new WASI(["wasmfga"], [], [new OpenFile(new File([])), ConsoleStdout.lineBuffered(console.log), ConsoleStdout.lineBuffered(console.error)], { debug: false });
start = performance.now();
const instance = await WebAssembly.instantiate(module, { wasi_snapshot_preview1: wasi.wasiImport });
wasi.initialize(instance);
const initializeMs = performance.now() - start;
const exp = instance.exports;
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
function callBytes(input) {
  const ptr = exp.alloc(input.length);
  if (!ptr) throw new Error("WASM allocation failed");
  new Uint8Array(exp.memory.buffer, ptr, input.length).set(input);
  const packed = exp.run();
  const response = JSON.parse(decoder.decode(new Uint8Array(exp.memory.buffer, Number(packed >> 32n), Number(packed & 0xffffffffn))));
  if (response.error) throw new Error(response.error);
  return response;
}
const call = request => callBytes(encoder.encode(JSON.stringify(request)));
const quantile = (sorted, q) => sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))];
const measurements = [];
for (const w of JSON.parse(await readFile(`${root}dist/benchmark-fixtures.json`))) {
  const db = new DatabaseSync(":memory:");
  db.exec("BEGIN");
  for (const s of call({ operation: "schema" }).statements) db.prepare(s.sql).run(...s.params);
  const store = "01ARZ3NDEKTSV4RRFFQ69G5FAV";
  const id = w.model.id;
  const modelBody = JSON.stringify(w.model);
  db.prepare("INSERT INTO wasmfga_models VALUES (?, ?, ?)").run(store, id, modelBody);
  const insert = db.prepare("INSERT INTO wasmfga_tuples VALUES (?, ?, ?, ?, ?)");
  for (const t of w.tuples) insert.run(store, t.object, t.relation, t.user, JSON.stringify(t));
  const request = { operation: "check", store_id: store, model_id: id, check: w.check };
  // The guest-only mode still parses/validates/hydrates the entire snapshot.
  // Pre-encoding removes host JSON serialization and SQL from this measurement.
  const rows = db.prepare("SELECT body FROM wasmfga_tuples WHERE store_id = ? ORDER BY object, relation, user LIMIT 10001").all(store).map(r => [r.body]);
  const hydrated = encoder.encode(JSON.stringify({ ...request, results: [[[modelBody]], rows] }));
  const run = mode === "guest" ? () => callBytes(hydrated) : () => {
    const plan = call(request);
    if (!plan.needs_results) throw new Error("expected SQL read plan");
    const results = plan.statements.map(s => {
      if (s.kind !== "query") throw new Error("unexpected check effect");
      return db.prepare(s.sql).all(...s.params).map(r => [r.body]);
    });
    return call({ ...request, results });
  };
  const factor = Number(process.env.WASMFGA_BENCH_SCALE ?? 1);
  const iterations = Math.max(10, Math.round((w.tuples.length >= 10000 ? 30 : w.tuples.length >= 1000 ? 100 : 500) * factor));
  const warmup = Math.max(10, Math.round((w.tuples.length >= 10000 ? 10 : 100) * factor));
  const verify = response => { if (response.allowed !== w.allowed) throw new Error(`${w.name}: incorrect benchmark decision`); };
  const firstStart = performance.now();
  const firstResult = run();
  const firstCheckUs = (performance.now() - firstStart) * 1000;
  verify(firstResult);
  for (let i = 0; i < warmup; i++) verify(run());
  const durations = [];
  for (let i = 0; i < iterations; i++) {
    const t = performance.now();
    const response = run();
    durations.push((performance.now() - t) * 1000);
    verify(response); // assert outside the timed interval, on every invocation
  }
  durations.sort((a, b) => a - b);
  measurements.push({ workload: w.name, tuples: w.tuples.length, snapshotInputBytes: hydrated.length,
    iterations, warmup, firstCheckUs, medianUs: quantile(durations, 0.5), p95Us: quantile(durations, 0.95),
    meanUs: durations.reduce((a, b) => a + b, 0) / durations.length,
    linearMemoryBytes: exp.memory.buffer.byteLength });
  db.exec("ROLLBACK");
  db.close();
}
await writeFile(output, JSON.stringify({ artifact, mode, compileMs, initializeMs, artifactBytes: bytes.length,
  peakRssKiB: process.resourceUsage().maxRSS, measurements }));
