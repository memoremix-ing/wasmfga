import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
export const root = fileURLToPath(new URL("../", import.meta.url));
export async function workloads() {
  const config = JSON.parse(await readFile(new URL("workloads.json", import.meta.url)));
  const model = JSON.parse(await readFile(new URL("../tests/fixtures/model.json", import.meta.url)));
  model.id = "01ARZ3NDEKTSV4RRFFQ69G5FAW";
  const corpus = JSON.parse(await readFile(new URL("../tests/fixtures/cases.json", import.meta.url)));
  const tuple = (object, relation, user) => ({ object, relation, user });
  return config.map(c => {
    const seed = c.case ? corpus.find(x => x.name === c.case) : corpus.find(x => x.name === "owner");
    if (!seed || seed.error) throw new Error(`invalid benchmark case ${c.name}`);
    const w = { ...structuredClone(seed), name: c.name, model: structuredClone(model) };
    if (c.kind === "userset_chain") {
      w.tuples = [tuple("document:1", "viewer", "group:0#member")];
      for (let i = 0; i < c.depth; i++) w.tuples.push(tuple(`group:${i}`, "member", i === c.depth - 1 ? "user:alice" : `group:${i + 1}#member`));
    } else if (c.kind === "parent_chain") {
      w.tuples = [tuple("document:1", "parent", "folder:0")];
      for (let i = 0; i < c.depth - 1; i++) w.tuples.push(tuple(`folder:${i}`, "parent", `folder:${i + 1}`));
      w.tuples.push(tuple(`folder:${c.depth - 1}`, "viewer", "user:alice"));
    }
    if (c.snapshot_size) {
      for (let i = w.tuples.length; i < c.snapshot_size; i++) w.tuples.push(tuple(`document:unrelated-${i}`, "owner", `user:unrelated-${i}`));
    }
    return w;
  });
}
if (process.argv[1] === fileURLToPath(import.meta.url)) {
  await writeFile(process.argv[2] ?? `${root}dist/benchmark-fixtures.json`, JSON.stringify(await workloads()));
}
