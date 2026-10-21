use anyhow::Result;
use rusqlite::Connection;
use serde_json::{json, Value};
use wasmfga::Engine;

fn request(op: &str) -> Value {
    json!({"operation":op,"store_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","model_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW"})
}

#[test]
fn conformance_and_borrowed_transaction() -> Result<()> {
    let wasm = std::fs::read(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../dist/wasmfga.wasm"
    ))?;
    let mut engine = Engine::new(&wasm)?;
    let model: Value = serde_json::from_str(include_str!("../../../tests/fixtures/model.json"))?;
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("../../../tests/fixtures/cases.json"))?;
    let mut db = Connection::open_in_memory()?;
    let mut tx = db.transaction()?;
    engine.run(&mut tx, request("schema"))?;
    let mut req = request("put_model");
    req["model"] = model;
    engine.run(&mut tx, req)?;
    tx.execute("CREATE TABLE app_audit (body TEXT)", [])?;
    tx.commit()?;
    for case in cases {
        let mut tx = db.transaction()?;
        if !case["tuples"].as_array().unwrap().is_empty() {
            let mut req = request("write");
            req["writes"] = case["tuples"].clone();
            engine.run(&mut tx, req)?;
        }
        let mut req = request("check");
        req["check"] = case["check"].clone();
        let result = engine.run(&mut tx, req);
        if case["error"].as_bool().unwrap_or(false) {
            assert!(result.is_err(), "{}", case["name"]);
        } else {
            assert_eq!(
                result?.allowed,
                case["allowed"].as_bool(),
                "{}",
                case["name"]
            );
        }
        tx.rollback()?;
    }
    let mut check = request("check");
    check["check"] =
        json!({"tuple_key":{"object":"document:1","relation":"can_view","user":"user:alice"}});
    for commit in [false, true] {
        let mut tx = db.transaction()?;
        let mut write = request("write");
        write["writes"] = json!([{"object":"document:1","relation":"owner","user":"user:alice"}]);
        engine.run(&mut tx, write)?;
        assert_eq!(engine.run(&mut tx, check.clone())?.allowed, Some(true));
        tx.execute("INSERT INTO app_audit VALUES ('application logic')", [])?;
        if commit {
            tx.commit()?
        } else {
            tx.rollback()?
        }
        let mut tx = db.transaction()?;
        assert_eq!(engine.run(&mut tx, check.clone())?.allowed, Some(commit));
        let n: i32 = tx.query_row("SELECT count(*) FROM app_audit", [], |r| r.get(0))?;
        assert_eq!(n, i32::from(commit));
        tx.rollback()?;
    }
    // The guest cannot cause a transaction commit, including on failed batch writes.
    let mut tx = db.transaction()?;
    let mut req = request("write");
    req["deletes"] = json!([{"object":"document:missing","relation":"owner","user":"user:alice"}]);
    assert!(engine.run(&mut tx, req).is_err());
    tx.rollback()?;
    Ok(())
}
