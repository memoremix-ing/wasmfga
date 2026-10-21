import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, rm, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { createClient } from "@libsql/client";
import { connect } from "@tursodatabase/database";
import { WasmFGA, preparedTransaction, libsqlTransaction } from "../dist/index.js";

const base = { store_id: "01ARZ3NDEKTSV4RRFFQ69G5FAV", model_id: "01ARZ3NDEKTSV4RRFFQ69G5FAW" };
const model = JSON.parse(await readFile("../../tests/fixtures/model.json"));
const cases = JSON.parse(await readFile("../../tests/fixtures/cases.json"));
const fga = await WasmFGA.create(await WebAssembly.compile(await readFile("../../dist/wasmfga.wasm")));

for (const backend of ["sqlite", "libsql", "turso"]) {
  test(`${backend}: OpenFGA conformance and caller-owned atomicity`, async () => {
    let begin, commit, rollback, raw, close;
    if (backend === "libsql") {
      const dir = await mkdtemp(join(tmpdir(), "wasmfga-"));
      const db = createClient({ url: `file:${join(dir, "test.db")}` });
      let active;
      begin = async () => { active = await db.transaction("write"); return libsqlTransaction(active); };
      raw = (sql) => active.execute(sql);
      commit = () => active.commit(); rollback = () => active.rollback(); close = async () => { db.close(); await rm(dir, { recursive: true, force: true }); };
    } else {
      const db = backend === "sqlite" ? new DatabaseSync(":memory:") : await connect(":memory:");
      raw = (sql) => db.exec(sql);
      begin = async () => { await raw("BEGIN"); return preparedTransaction(db); };
      commit = () => raw("COMMIT"); rollback = () => raw("ROLLBACK"); close = () => db.close();
    }
    try {
      let tx = await begin();
      await fga.run(tx, { operation: "schema" });
      await fga.run(tx, { ...base, operation: "put_model", model });
      await raw("CREATE TABLE app_audit (body TEXT)");
      await commit();
      for (const c of cases) {
        tx = await begin();
        try {
          if (c.tuples.length) await fga.run(tx, { ...base, operation: "write", writes: c.tuples });
          const result = fga.run(tx, { ...base, operation: "check", check: c.check });
          if (c.error) await assert.rejects(result, undefined, c.name);
          else assert.equal((await result).allowed, c.allowed, c.name);
        } finally { await rollback(); }
      }
      const check = cases.find(c => c.name === "owner").check;
      for (const persist of [false, true]) {
        tx = await begin();
        await fga.run(tx, { ...base, operation: "write", writes: [{ object: "document:1", relation: "owner", user: "user:alice" }] });
        assert.equal((await fga.run(tx, { ...base, operation: "check", check })).allowed, true);
        await raw("INSERT INTO app_audit VALUES ('application change')");
        if (persist) await commit(); else await rollback();
        tx = await begin();
        assert.equal((await fga.run(tx, { ...base, operation: "check", check })).allowed, persist);
        const count = await tx.query("SELECT CAST(count(*) AS TEXT) AS body FROM app_audit", []);
        assert.equal(count[0][0], persist ? "1" : "0");
        await rollback();
      }
    } finally { await close(); }
  });
}
