import { readFile, writeFile, mkdir } from "node:fs/promises";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { gzipSync } from "node:zlib";
import os from "node:os";
import { root } from "./workloads.mjs";
const sha = bytes => createHash("sha256").update(bytes).digest("hex");
const samples = Number(process.env.WASMFGA_BENCH_SAMPLES ?? 5);
if (!Number.isInteger(samples) || samples < 3) throw new Error("at least three independent process samples required");
const gate = JSON.parse(await readFile(`${root}dist/benchmark-gate.json`));
for (const artifact of Object.values(gate.artifacts)) {
  if (sha(await readFile(artifact.path)) !== artifact.sha256) throw new Error("artifact differs from correctness gate; rerun make benchmark");
}
if (sha(await readFile(`${root}dist/benchmark-fixtures.json`)) !== gate.fixtureSha256) throw new Error("fixtures changed since oracle gate");
const results = [];
for (let sample = 0; sample < samples; sample++) {
  for (const mode of ["guest", "sqlite"]) {
    const order = (sample + (mode === "sqlite" ? 1 : 0)) % 2 ? ["rust", "go"] : ["go", "rust"];
    for (const engine of order) {
      console.log(`Sample ${sample + 1}/${samples} ${mode} ${engine}`);
      const output = `${root}dist/bench-worker-${engine}-${mode}-${sample}.json`;
      const result = spawnSync(process.execPath, [`${root}benchmarks/worker.mjs`, gate.artifacts[engine].path, mode, output], { cwd: root, stdio: "inherit", env: process.env });
      if (result.status !== 0) process.exit(result.status ?? 1);
      results.push({ engine, sample, ...JSON.parse(await readFile(output)) });
    }
  }
}
for (const artifact of Object.values(gate.artifacts)) {
  if (sha(await readFile(artifact.path)) !== artifact.sha256) throw new Error("artifact changed during benchmark");
}
const median = list => { const a = [...list].sort((x, y) => x - y); return a[Math.floor(a.length / 2)]; };
const artifacts = {};
for (const engine of ["go", "rust"]) {
  const bytes = await readFile(gate.artifacts[engine].path);
  const runs = results.filter(r => r.engine === engine);
  artifacts[engine] = { sha256: sha(bytes), bytes: bytes.length, gzipBytes: gzipSync(bytes, { level: 9 }).length,
    compileMs: median(runs.map(r => r.compileMs)), initializeMs: median(runs.map(r => r.initializeMs)),
    firstSqliteCheckMs: median(runs.filter(r => r.mode === "sqlite").map(r => r.measurements[0].firstCheckUs / 1000)),
    maxLinearMemoryBytes: Math.max(...runs.flatMap(r => r.measurements.map(m => m.linearMemoryBytes))),
    maxProcessRssKiB: Math.max(...runs.map(r => r.peakRssKiB)) };
}
const rows = [];
for (const mode of ["guest", "sqlite"]) {
  const workloads = results.find(r => r.mode === mode).measurements.map(m => m.workload);
  for (const workload of workloads) {
    const row = { mode, workload };
    for (const engine of ["go", "rust"]) {
      const measures = results.filter(r => r.engine === engine && r.mode === mode).map(r => r.measurements.find(m => m.workload === workload));
      const medians = measures.map(m => m.medianUs);
      row[engine] = { medianUs: median(medians), p95Us: median(measures.map(m => m.p95Us)),
        minSampleMedianUs: Math.min(...medians), maxSampleMedianUs: Math.max(...medians),
        iterationsPerSample: measures[0].iterations, tuples: measures[0].tuples, snapshotInputBytes: measures[0].snapshotInputBytes };
    }
    if (row.go.snapshotInputBytes !== row.rust.snapshotInputBytes) throw new Error("unequal benchmark inputs");
    row.speedup = row.go.medianUs / row.rust.medianUs;
    rows.push(row);
  }
}
const command = (file, args) => execFileSync(file, args, { encoding: "utf8", cwd: root }).trim();
const report = { measuredAt: new Date().toISOString(), samples, metadata: { platform: os.platform(), arch: os.arch(), release: os.release(), cpu: os.cpus()[0].model,
  logicalCpus: os.cpus().length, totalMemoryBytes: os.totalmem(), node: process.version, v8: process.versions.v8,
  go: command("go", ["version"]), rust: command("rustc", ["--version"]), jjChange: command("jj", ["log", "-r", "@", "--no-graph", "-T", "change_id"]),
  goBuild: "GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -buildmode=c-shared",
  rustBuild: "cargo build --locked --release --target wasm32-wasip1 (opt-level=3, thin LTO, codegen-units=1, strip=true)" },
  methodology: { runtime: "Node/V8; identical synchronous benchmark host, WASI shim, linear-memory ABI, SQL protocol, model and sorted snapshot bytes",
    guest: "One hydrated phase-2 ABI call; pre-encoded input. Includes guest JSON parsing, model/CEL validation, tuple validation/indexing and resolution; output decoding included. Excludes host SQL and input encoding.",
    sqlite: "Complete two-call Check inside an existing SQLite in-memory transaction; includes SQL preparation/reads and host JSON encoding/decoding. Excludes fixture setup and transaction begin/commit/rollback.",
    statistics: "Median of per-process medians across fresh processes. p95 is median of process p95 values. Process order alternates. Every response is checked. Warmups and iteration counts are recorded. No persistent guest authorization caches.",
    startup: "Fresh-process WebAssembly.compile and instantiate+_initialize; excludes artifact I/O, process launch and network. V8 may defer optimization to warmup. First SQLite Check is reported separately after schema setup, before any model/check warmup.",
    memory: "Max observed linear memory across all workloads, and peak RSS of a complete worker process (includes Node/V8/SQLite/JIT). Neither is per-check allocation.",
    limitations: "Rust is an experimental independent evaluator with a conservative scalar CEL subset and restricted numeric-string conversion; passing tested corpora is not full OpenFGA or CEL conformance. Results compare these two ports, including differing validation/telemetry/runtime designs, rather than Go versus Rust universally. One local machine; no remote database/network or durable commit costs measured." },
  gate: { ...gate, artifacts: Object.fromEntries(Object.entries(gate.artifacts).map(([name, value]) => [name, { ...value, path: value.path.replace(root, "") }])) }, artifacts, rows, raw: results.map(r => ({...r, artifact:r.artifact.replace(root, "")})) };
await mkdir(`${root}benchmarks/results`, { recursive: true });
await writeFile(`${root}benchmarks/results/latest.json`, JSON.stringify(report, null, 2) + "\n");
console.log(JSON.stringify({ artifacts, sqlite: rows.filter(r => r.mode === "sqlite").map(r => ({ workload: r.workload, goUs: r.go.medianUs, rustUs: r.rust.medianUs, speedup: r.speedup })) }, null, 2));
