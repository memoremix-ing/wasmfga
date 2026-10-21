import { readFile, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { root, workloads } from "./workloads.mjs";
const sha = bytes => createHash("sha256").update(bytes).digest("hex");
const fixturePath = `${root}dist/benchmark-fixtures.json`;
await writeFile(fixturePath, JSON.stringify(await workloads()));
const artifacts = {};
for (const [name, file] of [["go", "wasmfga.wasm"], ["rust", "wasmfga-rust.wasm"]]) {
  const path = `${root}dist/${file}`;
  const before = sha(await readFile(path));
  console.log(`Oracle gate: ${name}`);
  const result = spawnSync("go", ["test", "./tests", "-count=1"], {
    cwd: root, stdio: "inherit", env: { ...process.env, GOTOOLCHAIN: "local", WASMFGA_WASM: path, WASMFGA_BENCH_FIXTURES: fixturePath },
  });
  if (result.status !== 0) process.exit(result.status ?? 1);
  if (sha(await readFile(path)) !== before) throw new Error("artifact changed during correctness gate");
  artifacts[name] = { path, sha256: before };
}
await writeFile(`${root}dist/benchmark-gate.json`, JSON.stringify({ completedAt: new Date().toISOString(), artifacts,
  fixtureSha256: sha(await readFile(fixturePath)), tests: "516 upstream assertions, 21 shared cases, 100 Hegel histories per artifact, model-validation mutations, cross-engine transaction and failure/isolation checks, all generated benchmark workloads" }, null, 2));
