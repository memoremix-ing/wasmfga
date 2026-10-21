import { readFile, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { root } from "../workloads.mjs";
const sha = bytes => createHash("sha256").update(bytes).digest("hex");
const fixturePath = `${root}dist/native/fixtures.json`;
const binaries = Object.fromEntries(await Promise.all(["go","rust"].map(async engine => {
  const path=`${root}dist/native/${engine}-bench`;return [engine,{path,sha256:sha(await readFile(path))}];
})));
const execute=(file,args)=>{const r=spawnSync(file,args,{cwd:root,stdio:"inherit"});if(r.status!==0)throw new Error(`native workload gate failed: ${file} ${args.join(" ")}`)};
execute(binaries.go.path,["--oracle","--fixtures",fixturePath,"--output",`${root}dist/native/oracle.json`]);
const checks=[];
for(const [engine,mode] of [["go","json"],["rust","json"],["go","sqlite"],["rust","sqlite"],["upstream","sqlite"],["upstream-cached","sqlite"]]){
  const binary=engine==="rust"?binaries.rust:binaries.go;
  execute(binary.path,["--engine",engine,"--mode",mode,"--gate","--fixtures",fixturePath,"--output",`${root}dist/native/gate-${engine}-${mode}.json`]);
  checks.push({engine,mode});
}
for(const b of Object.values(binaries))if(sha(await readFile(b.path))!==b.sha256)throw new Error("native binary changed during gate");
await writeFile(`${root}dist/native/gate.json`,JSON.stringify({completedAt:new Date().toISOString(),binaries,fixturePath,fixtureSha256:sha(await readFile(fixturePath)),checks,
  oracle:JSON.parse(await readFile(`${root}dist/native/oracle.json`)),
  scope:"Ten shared valid Check workloads only. This is not unrestricted conformance; the Rust port retains the known CEL gaps and contextual-duplicate false allow documented in docs/conformance-audit.md."},null,2));
