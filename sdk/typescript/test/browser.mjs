import { WasmFGA } from "../dist/index.js";

async function run() {
  const [model, cases, wasm, sqlite] = await Promise.all([
    fetch("/tests/fixtures/model.json").then(r => r.json()),
    fetch("/tests/fixtures/cases.json").then(r => r.json()),
    fetch("/dist/wasmfga.wasm").then(r => r.arrayBuffer()),
    globalThis.initSqlJs({ locateFile: file => `/sdk/typescript/node_modules/sql.js/dist/${file}` }),
  ]);
  const fga = await WasmFGA.create(wasm);
  const db = new sqlite.Database();
  const base = { store_id: "01ARZ3NDEKTSV4RRFFQ69G5FAV", model_id: "01ARZ3NDEKTSV4RRFFQ69G5FAW" };
  const assert = (ok, message) => { if (!ok) throw new Error(message); };
  const tx = {
    async query(sql, params) { return db.exec(sql, params)[0]?.values ?? []; },
    async execute(sql, params) { db.run(sql, params); return db.getRowsModified(); },
  };
  let passed = 0;
  try {
    db.run("BEGIN");
    await fga.run(tx, { operation: "schema" });
    await fga.run(tx, { ...base, operation: "put_model", model });
    db.run("CREATE TABLE app_audit (body TEXT)");
    db.run("COMMIT");
    for (const c of cases) {
      db.run("BEGIN");
      try {
        if (c.tuples.length) await fga.run(tx, { ...base, operation: "write", writes: c.tuples });
        let response, error;
        try { response = await fga.run(tx, { ...base, operation: "check", check: c.check }); } catch (e) { error = e; }
        assert(Boolean(error) === Boolean(c.error), `${c.name}: unexpected error ${error}`);
        if (!error) assert(response.allowed === c.allowed, `${c.name}: wrong decision`);
        passed++;
      } finally { db.run("ROLLBACK"); }
    }
    const check = cases.find(c => c.name === "owner").check;
    for (const commit of [false, true]) {
      db.run("BEGIN");
      await fga.run(tx, { ...base, operation: "write", writes: [{ object: "document:1", relation: "owner", user: "user:alice" }] });
      assert((await fga.run(tx, { ...base, operation: "check", check })).allowed, "uncommitted grant invisible");
      db.run("INSERT INTO app_audit VALUES ('browser app logic')");
      db.run(commit ? "COMMIT" : "ROLLBACK");
      db.run("BEGIN");
      assert((await fga.run(tx, { ...base, operation: "check", check })).allowed === commit, "transaction state escaped");
      assert(db.exec("SELECT count(*) FROM app_audit")[0].values[0][0] === (commit ? 1 : 0), "application change not atomic");
      db.run("ROLLBACK");
      passed++;
    }
    return { passed, error: null, userAgent: navigator.userAgent };
  } finally { db.close(); }
}

globalThis.testResult = null;
run().then(result => {
  globalThis.testResult = result;
  document.getElementById("result").textContent = JSON.stringify(result, null, 2);
}).catch(error => {
  globalThis.testResult = { error: error.stack || String(error) };
  document.getElementById("result").textContent = globalThis.testResult.error;
});
