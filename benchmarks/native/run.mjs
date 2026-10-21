import { readFile, writeFile, mkdir } from "node:fs/promises";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import os from "node:os";
import { root } from "../workloads.mjs";
const sha=b=>createHash("sha256").update(b).digest("hex");
const samples=Number(process.env.WASMFGA_BENCH_SAMPLES??5),scale=Number(process.env.WASMFGA_BENCH_SCALE??1);
if(!Number.isInteger(samples)||samples<3||!Number.isFinite(scale)||scale<=0)throw new Error("invalid samples/scale");
const gate=JSON.parse(await readFile(`${root}dist/native/gate.json`));
const verify=async()=>{
  for(const b of Object.values(gate.binaries))if(sha(await readFile(b.path))!==b.sha256)throw new Error("native binary differs from oracle gate");
  if(sha(await readFile(gate.fixturePath))!==gate.fixtureSha256)throw new Error("fixtures differ from oracle gate");
};
await verify();
const raw=[];
for(let sample=0;sample<samples;sample++){
  for(const mode of ["json","sqlite"]){
    const engines=mode==="json"?["go","rust"]:["go","rust","upstream","upstream-cached"];
    const order=engines.slice(sample%engines.length).concat(engines.slice(0,sample%engines.length));
    for(const engine of order){
      console.log(`Native sample ${sample+1}/${samples}: ${mode} ${engine}`);
      const output=`${root}dist/native/sample-${sample}-${engine}-${mode}.json`;
      const r=spawnSync(gate.binaries[engine==="rust"?"rust":"go"].path,["--engine",engine,"--mode",mode,"--fixtures",gate.fixturePath,"--output",output,"--scale",String(scale)],{cwd:root,stdio:"inherit"});
      if(r.status!==0)throw new Error("native measurement failed");
      raw.push({sample,...JSON.parse(await readFile(output))});
    }
  }
}
await verify();
const median=a=>[...a].sort((a,b)=>a-b)[Math.floor(a.length/2)];
const rows=[];
for(const mode of ["json","sqlite"]){
  for(const first of raw.find(r=>r.mode===mode).measurements){
    const row={mode,workload:first.workload,tuples:first.tuples,engines:{}};
    for(const engine of (mode==="json"?["go","rust"]:["go","rust","upstream","upstream-cached"])){
      const m=raw.filter(r=>r.mode===mode&&r.engine===engine).map(r=>r.measurements.find(m=>m.workload===first.workload));
      if(m.some(m=>m.snapshotInputBytes!==first.snapshotInputBytes))throw new Error("unequal native snapshot bytes");
      row.engines[engine]={medianUs:median(m.map(m=>m.medianUs)),p95Us:median(m.map(m=>m.p95Us)),minSampleMedianUs:Math.min(...m.map(m=>m.medianUs)),maxSampleMedianUs:Math.max(...m.map(m=>m.medianUs)),iterationsPerSample:m[0].iterations,firstCheckUs:median(m.map(m=>m.firstCheckUs)),prepareUs:median(m.map(m=>m.prepareUs))};
    }
    rows.push(row);
  }
}
const command=(file,args)=>execFileSync(file,args,{cwd:root,encoding:"utf8"}).trim();
const labels={go:"Go port (native snapshots)",rust:"Rust port (native snapshots)",upstream:"Original Go checker (transaction SQL)","upstream-cached":"Original Go checker (compiled model reused)"};
const report={measuredAt:new Date().toISOString(),samples,scale,labels,metadata:{platform:os.platform(),arch:os.arch(),release:os.release(),cpu:os.cpus()[0].model,logicalCpus:os.cpus().length,
  go:command("go",["version"]),rust:command("rustc",["--version"]),node:process.version,
  jjChange:command("jj",["log","-r","@","--no-graph","-T","change_id"]),upstream:JSON.parse(await readFile(`${root}upstream.json`)),
  goBuild:"go build -buildvcs=false -trimpath -ldflags='-s -w'; default native optimization; no WASM engine",
  rustBuild:"cargo build --locked --release; native opt-level=3, thin LTO, codegen-units=1, strip=true, panic=abort",
  drivers:Object.fromEntries(["go","rust"].map(e=>{const r=raw.find(r=>r.engine===e);return[e,{driver:r.driver,sqliteVersion:r.sqliteVersion,gomaxprocs:r.gomaxprocs}]}))},
  methodology:{sqlite:"Native library calls in an already-open in-memory SQLite transaction. Includes SQL preparation, reads, stored JSON decoding, validation and evaluation. No outer request/response JSON serialization, WASM, IPC, network or transaction begin/commit costs. Native Go uses a typed Request; native Rust uses a serde_json::Value envelope.",
    json:"Pre-encoded identical hydrated input bytes passed directly to each native Run function, then response decoding. Includes model/CEL validation, snapshot construction and evaluation. Excludes SQL and input encoding. Matches the scope of the earlier hydrated WASM invocation more closely than the structured native library mode.",
    upstream:"Unmodified upstream commands.NewCheckCommand and graph.NewLocalChecker, bound through a new transaction-only RelationshipTupleReader over wasmfga's two-table schema. Accepts a native protobuf CheckRequest pre-decoded outside timing. Reads relevant edges directly; no complete-store hydration. Not the standalone HTTP/gRPC server or stock OpenFGA SQL backend. Same dispatch depth 25 and breadth 1 as the Go snapshot port. No tuple/decision cache.",
    cached:"The separate upstream-cached variant prepares the immutable model and resolver once per workload/transaction outside timing; preparation is recorded. The other three SQLite variants reload/validate models per Check. This is an application configuration difference, not an isolated language comparison.",
    statistics:"Five fresh processes per engine/mode by default, sequential execution, rotating engine order. Median of process medians and median of process p95 values; raw samples and range of medians retained. Small workloads: 500 warmups/5000 measured checks; 1000 tuples: 100/500; 10000 tuples: 10/50. Counts are multiplied by the recorded scale. Every decision verified outside its timed interval. No GC forcing, CPU pinning or profiler.",
    limitations:"Rust remains experimental with known conformance gaps including a contextual-duplicate false allow. All measured workloads pass the native OpenFGA oracle; that does not establish general equivalence. Go snapshot hydration still uses quadratic upstream MemoryBackend.Write; direct upstream SQL avoids it. Go and Rust use different SQLite bindings/builds. One in-flight Check at a time; Go may dispatch graph branches using goroutines. One machine, no remote or durable-commit costs. Earlier WASM results were collected in a separate run; cross-run ratios are descriptive, not a simultaneous controlled comparison."},
  gate,rows,raw};
await mkdir(`${root}benchmarks/results`,{recursive:true});
await writeFile(`${root}benchmarks/results/native-latest.json`,JSON.stringify(report,null,2)+"\n");
console.log(JSON.stringify(rows.filter(r=>r.mode==="sqlite").map(r=>({workload:r.workload,...Object.fromEntries(Object.entries(r.engines).map(([e,m])=>[e,m.medianUs]))})),null,2));
