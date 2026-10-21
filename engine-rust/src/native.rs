//! Native library interface. The host owns the transaction lifecycle.
use crate::{model::ensure, process_ref, Result};
use serde::Deserialize;
use serde_json::Value;

/// Borrow an active transaction exclusively. Roll back after ANY error; a batch
/// can have executed earlier statements before a later statement fails.
pub trait Transaction {
    fn query(&mut self, sql: &str, params: &[String]) -> Result<Vec<Vec<String>>>;
    fn execute(&mut self, sql: &str, params: &[String]) -> Result<u64>;
}

#[cfg(feature = "sqlite")]
impl Transaction for rusqlite::Transaction<'_> {
    fn query(&mut self, sql: &str, params: &[String]) -> Result<Vec<Vec<String>>> {
        let mut stmt = self.prepare(sql).map_err(|e| e.to_string())?;
        let rows = stmt
            .query_map(rusqlite::params_from_iter(params), |row| {
                Ok(vec![row.get(0)?])
            })
            .map_err(|e| e.to_string())?;
        rows.collect::<rusqlite::Result<Vec<_>>>()
            .map_err(|e| e.to_string())
    }
    fn execute(&mut self, sql: &str, params: &[String]) -> Result<u64> {
        rusqlite::Connection::execute(self, sql, rusqlite::params_from_iter(params))
            .map(|n| n as u64)
            .map_err(|e| e.to_string())
    }
}

#[derive(Deserialize)]
struct Effect {
    kind: String,
    sql: String,
    params: Vec<String>,
    expected_changes: Option<u64>,
}

/// No WASM engine or outer JSON round trip. The binding exposes no lifecycle
/// methods. Like process_ref, this structured API has no serialized-input limit.
pub fn run(tx: &mut impl Transaction, mut request: Value) -> Result<Value> {
    request
        .as_object_mut()
        .ok_or("request must be an object")?
        .remove("results");
    for _ in 0..2 {
        let response = process_ref(&request)?;
        let effects: Vec<Effect> = serde_json::from_value(
            response
                .get("statements")
                .cloned()
                .unwrap_or(Value::Array(vec![])),
        )
        .map_err(|e| e.to_string())?;
        let mut results = Vec::new();
        for effect in effects {
            match effect.kind.as_str() {
                "query" => results.push(tx.query(&effect.sql, &effect.params)?),
                "execute" => {
                    let changed = tx.execute(&effect.sql, &effect.params)?;
                    if let Some(expected) = effect.expected_changes {
                        ensure(
                            changed == expected,
                            "unexpected row count; caller must roll back",
                        )?;
                    }
                }
                _ => return Err("unknown SQL effect".into()),
            }
        }
        if response["needs_results"] != true {
            return Ok(response);
        }
        request["results"] = serde_json::to_value(results).map_err(|e| e.to_string())?;
    }
    Err("native protocol exceeded two rounds".into())
}
