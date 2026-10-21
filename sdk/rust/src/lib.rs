//! WASI evaluator using an already-open host transaction. No lifecycle SQL is emitted.
use anyhow::{bail, ensure, Context, Result};
use serde::Deserialize;
use serde_json::Value;
use wasmtime::{Engine as WasmEngine, Linker, Memory, Module, Store, TypedFunc};
use wasmtime_wasi::{
    p1::{self, WasiP1Ctx},
    WasiCtxBuilder,
};

/// Caller owns begin/commit/rollback. Roll back the transaction on ANY error.
pub trait Transaction {
    fn query(&mut self, sql: &str, params: &[String]) -> Result<Vec<Vec<String>>>;
    fn execute(&mut self, sql: &str, params: &[String]) -> Result<u64>;
}

impl Transaction for rusqlite::Transaction<'_> {
    fn query(&mut self, sql: &str, params: &[String]) -> Result<Vec<Vec<String>>> {
        let mut stmt = self.prepare(sql)?;
        let rows = stmt.query_map(rusqlite::params_from_iter(params), |row| {
            Ok(vec![row.get(0)?])
        })?;
        Ok(rows.collect::<rusqlite::Result<Vec<_>>>()?)
    }
    fn execute(&mut self, sql: &str, params: &[String]) -> Result<u64> {
        Ok(rusqlite::Connection::execute(self, sql, rusqlite::params_from_iter(params))? as u64)
    }
}

#[derive(Deserialize, Debug)]
struct Statement {
    kind: String,
    sql: String,
    params: Vec<String>,
    expected_changes: Option<u64>,
}
#[derive(Deserialize, Debug)]
pub struct Response {
    pub allowed: Option<bool>,
    #[serde(default)]
    needs_results: bool,
    #[serde(default)]
    statements: Vec<Statement>,
    error: Option<String>,
}

pub struct Engine {
    store: Store<WasiP1Ctx>,
    memory: Memory,
    alloc: TypedFunc<u32, u32>,
    run: TypedFunc<(), u64>,
}

impl Engine {
    pub fn new(wasm: &[u8]) -> Result<Self> {
        let engine = WasmEngine::default();
        let module = Module::new(&engine, wasm)?;
        let mut linker = Linker::new(&engine);
        p1::add_to_linker_sync(&mut linker, |ctx| ctx)?;
        // No inherited files, environment, network, or filesystem capabilities.
        let mut store = Store::new(&engine, WasiCtxBuilder::new().arg("wasmfga").build_p1());
        let instance = linker.instantiate(&mut store, &module)?;
        instance
            .get_typed_func::<(), ()>(&mut store, "_initialize")?
            .call(&mut store, ())?;
        let memory = instance
            .get_memory(&mut store, "memory")
            .context("missing memory")?;
        let alloc = instance.get_typed_func(&mut store, "alloc")?;
        let run = instance.get_typed_func(&mut store, "run")?;
        Ok(Self {
            store,
            memory,
            alloc,
            run,
        })
    }

    fn call(&mut self, request: &Value) -> Result<Response> {
        let input = serde_json::to_vec(request)?;
        ensure!(input.len() <= 16 << 20, "input exceeds 16 MiB");
        let ptr = self.alloc.call(&mut self.store, input.len() as u32)?;
        ensure!(ptr != 0, "WASM allocation failed");
        self.memory.write(&mut self.store, ptr as usize, &input)?;
        let packed = self.run.call(&mut self.store, ())?;
        let mut output = vec![0; (packed as u32) as usize];
        self.memory
            .read(&self.store, (packed >> 32) as usize, &mut output)?;
        let response: Response = serde_json::from_slice(&output)?;
        if let Some(err) = response.error.as_ref() {
            bail!("wasmfga: {err}")
        }
        Ok(response)
    }

    /// Borrows a transaction exclusively. The caller must roll back on any error,
    /// including errors after an earlier statement in a batch succeeded.
    pub fn run(&mut self, tx: &mut impl Transaction, mut request: Value) -> Result<Response> {
        request
            .as_object_mut()
            .context("request must be an object")?
            .remove("results");
        for _ in 0..2 {
            let response = self.call(&request)?;
            let mut results = Vec::new();
            for effect in &response.statements {
                match effect.kind.as_str() {
                    "query" => results.push(tx.query(&effect.sql, &effect.params)?),
                    "execute" => {
                        let changed = tx.execute(&effect.sql, &effect.params)?;
                        if let Some(expected) = effect.expected_changes {
                            ensure!(
                                changed == expected,
                                "expected {expected} changes, got {changed}; caller must roll back"
                            );
                        }
                    }
                    _ => bail!("unknown SQL effect"),
                }
            }
            if !response.needs_results {
                return Ok(response);
            }
            request["results"] = serde_json::to_value(results)?;
        }
        bail!("WASM protocol exceeded two rounds")
    }
}
