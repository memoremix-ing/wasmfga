#![cfg(feature = "sqlite")]
use rusqlite::Connection;
use serde_json::{json, Value};
use wasmfga_engine::native;

fn database() -> Connection {
    let mut db = Connection::open_in_memory().unwrap();
    let mut tx = db.transaction().unwrap();
    native::run(&mut tx, json!({"operation":"schema"})).unwrap();
    let model: Value =
        serde_json::from_str(include_str!("../../tests/fixtures/model.json")).unwrap();
    native::run(
        &mut tx,
        json!({"operation":"put_model","store_id":"s","model_id":"m","model":model}),
    )
    .unwrap();
    tx.execute("CREATE TABLE app_audit (body TEXT NOT NULL)", [])
        .unwrap();
    tx.commit().unwrap();
    db
}
fn request(op: &str) -> Value {
    json!({"operation":op,"store_id":"s","model_id":"m"})
}
fn grant() -> Value {
    json!({"object":"document:1","relation":"owner","user":"user:alice"})
}
fn check(tx: &mut rusqlite::Transaction<'_>) -> bool {
    let mut r = request("check");
    r["check"] =
        json!({"tuple_key":{"object":"document:1","relation":"can_view","user":"user:alice"}});
    native::run(tx, r).unwrap()["allowed"].as_bool().unwrap()
}

#[test]
fn caller_commits_and_rolls_back_application_and_authorization_together() {
    for commit in [false, true] {
        let mut db = database();
        let mut tx = db.transaction().unwrap();
        let mut r = request("write");
        r["writes"] = json!([grant()]);
        native::run(&mut tx, r).unwrap();
        assert!(check(&mut tx));
        tx.execute("INSERT INTO app_audit VALUES ('authorized operation')", [])
            .unwrap();
        if commit {
            tx.commit().unwrap()
        } else {
            tx.rollback().unwrap()
        }
        let mut tx = db.transaction().unwrap();
        assert_eq!(check(&mut tx), commit);
        let count: i64 = tx
            .query_row("SELECT count(*) FROM app_audit", [], |r| r.get(0))
            .unwrap();
        assert_eq!(count, i64::from(commit));
        tx.rollback().unwrap();
    }
}

#[test]
fn sql_failure_requires_host_rollback_and_restores_all_changes() {
    let mut db = database();
    let mut tx = db.transaction().unwrap();
    tx.execute("INSERT INTO app_audit VALUES ('before batch')", [])
        .unwrap();
    let mut r = request("write");
    r["writes"] = json!([grant()]);
    native::run(&mut tx, r).unwrap();
    let mut r = request("write");
    r["writes"] = json!([{"object":"document:2","relation":"owner","user":"user:bob"},grant()]);
    assert!(native::run(&mut tx, r).is_err());
    let count: i64 = tx
        .query_row("SELECT count(*) FROM wasmfga_tuples", [], |r| r.get(0))
        .unwrap();
    assert_eq!(
        count, 2,
        "earlier statement remains tentative until host rollback"
    );
    tx.rollback().unwrap();
    for table in ["app_audit", "wasmfga_tuples"] {
        let count: i64 = db
            .query_row(&format!("SELECT count(*) FROM {table}"), [], |r| r.get(0))
            .unwrap();
        assert_eq!(count, 0);
    }
}

#[test]
fn native_sqlite_shared_corpus() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("../../tests/fixtures/cases.json")).unwrap();
    for c in cases {
        let mut db = database();
        let mut tx = db.transaction().unwrap();
        if !c["tuples"].as_array().unwrap().is_empty() {
            let mut r = request("write");
            r["writes"] = c["tuples"].clone();
            native::run(&mut tx, r).unwrap();
        }
        let mut r = request("check");
        r["check"] = c["check"].clone();
        let result = native::run(&mut tx, r);
        if c["error"] == true {
            assert!(result.is_err(), "{}", c["name"])
        } else {
            assert_eq!(result.unwrap()["allowed"], c["allowed"], "{}", c["name"])
        }
        tx.rollback().unwrap();
    }
}
