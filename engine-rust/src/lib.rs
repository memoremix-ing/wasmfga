//! Independent Rust port of the snapshot/SQL-effect OpenFGA Check evaluator.
mod conditions;
mod evaluate;
mod model;
pub mod native;
use model::{ensure, validate_context, Model, Tuple, ValidatedModel};
use serde_json::{json, Map, Value};
use std::collections::BTreeSet;
pub type Error = String;
pub type Result<T> = std::result::Result<T, Error>;
pub const MAX_INPUT: usize = 16 << 20;
const MAX_TUPLES: usize = 10000;
fn text<'a>(v: &'a Value, key: &str) -> Result<&'a str> {
    v.get(key)
        .and_then(Value::as_str)
        .ok_or_else(|| format!("{key} must be a string"))
}
fn array<'a>(v: &'a Value, key: &str) -> Result<&'a [Value]> {
    match v.get(key) {
        None | Some(Value::Null) => Ok(&[]),
        Some(Value::Array(a)) => Ok(a),
        _ => Err(format!("{key} must be an array")),
    }
}
fn statement(kind: &str, sql: &str, params: Vec<String>) -> Value {
    json!({"kind":kind,"sql":sql,"params":params})
}
fn parsed_model(v: Value) -> Result<ValidatedModel> {
    ValidatedModel::new(serde_json::from_value(v).map_err(|e| e.to_string())?)
}
fn tuple(v: &Value) -> Result<Tuple> {
    serde_json::from_value(v.clone()).map_err(|e| e.to_string())
}
fn alias<'a>(v: &'a Value, snake: &str, camel: &str) -> &'a Value {
    v.get(snake)
        .or_else(|| v.get(camel))
        .unwrap_or(&Value::Null)
}

pub fn process(r: Value) -> Result<Value> {
    process_ref(&r)
}

/// Native borrowed-input entry point. It avoids cloning/serializing the outer
/// envelope; stored model/tuple bodies retain the common JSON representation.
pub fn process_ref(r: &Value) -> Result<Value> {
    let dialect = r.get("dialect").and_then(Value::as_str).unwrap_or("");
    ensure(
        matches!(dialect, "" | "sqlite" | "turso"),
        "unsupported dialect (sqlite/turso only)",
    )?;
    let op = text(r, "operation")?;
    if op == "schema" {
        return Ok(json!({"statements":[
            statement("execute","CREATE TABLE IF NOT EXISTS wasmfga_models (store_id TEXT NOT NULL, model_id TEXT NOT NULL, body TEXT NOT NULL, PRIMARY KEY (store_id, model_id))",vec![]),
            statement("execute","CREATE TABLE IF NOT EXISTS wasmfga_tuples (store_id TEXT NOT NULL, object TEXT NOT NULL, relation TEXT NOT NULL, user TEXT NOT NULL, body TEXT NOT NULL, PRIMARY KEY (store_id, object, relation, user))",vec![])
        ]}));
    }
    let store = text(r, "store_id")?;
    let id = text(r, "model_id")?;
    ensure(
        !store.is_empty() && store.len() <= 256 && !id.is_empty() && id.len() <= 256,
        "store_id and model_id must contain 1..256 bytes",
    )?;
    if op == "put_model" {
        let mut raw = r["model"].clone();
        ensure(raw.is_object(), "model must be an object")?;
        raw["id"] = json!(id);
        let m = parsed_model(raw)?;
        return Ok(
            json!({"statements":[statement("execute","INSERT INTO wasmfga_models (store_id, model_id, body) VALUES (?, ?, ?)",vec![store.into(),id.into(),serde_json::to_string(&m.model).map_err(|e|e.to_string())?])]}),
        );
    }
    ensure(op == "check" || op == "write", "unknown operation")?;
    if r.get("results").is_none_or(Value::is_null) {
        let mut statements = vec![statement(
            "query",
            "SELECT body FROM wasmfga_models WHERE store_id = ? AND model_id = ?",
            vec![store.into(), id.into()],
        )];
        if op == "check" {
            statements.push(statement("query","SELECT body FROM wasmfga_tuples WHERE store_id = ? ORDER BY object, relation, user LIMIT 10001",vec![store.into()]));
        }
        return Ok(json!({"needs_results":true,"statements":statements}));
    }
    let results: Vec<Vec<Vec<String>>> =
        serde_json::from_value(r["results"].clone()).map_err(|e| e.to_string())?;
    ensure(
        results.len() == if op == "check" { 2 } else { 1 },
        "incorrect number of SQL result sets",
    )?;
    ensure(
        results[0].len() == 1 && results[0][0].len() == 1,
        "authorization model not found or invalid result shape",
    )?;
    let model: Model = serde_json::from_str(&results[0][0][0]).map_err(|e| e.to_string())?;
    ensure(model.id == id, "model ID does not match request")?;
    let model = ValidatedModel::new(model)?;
    if op == "write" {
        return plan_write(r, store, &model);
    }
    ensure(
        results[1].len() <= MAX_TUPLES,
        "store snapshot exceeds 10000 tuples",
    )?;
    let mut stored = Vec::new();
    for row in &results[1] {
        ensure(row.len() == 1, "invalid tuple result shape")?;
        stored.push(serde_json::from_str::<Tuple>(&row[0]).map_err(|e| e.to_string())?);
    }
    let q = &r["check"];
    let key = tuple(alias(q, "tuple_key", "tupleKey"))?;
    ensure(
        key.condition.is_none(),
        "check tuple key must not contain a condition",
    )?;
    let contextual = alias(q, "contextual_tuples", "contextualTuples");
    let ctx_keys = alias(contextual, "tuple_keys", "tupleKeys");
    let contextual: Vec<Tuple> = if ctx_keys.is_null() {
        vec![]
    } else {
        serde_json::from_value(ctx_keys.clone()).map_err(|e| e.to_string())?
    };
    ensure(contextual.len() <= 100, "contextual tuples exceed 100")?;
    for t in &contextual {
        model.tuple(t)?;
    }
    if let Some(c) = q.get("consistency") {
        ensure(
            matches!(
                c.as_str(),
                Some("UNSPECIFIED" | "MINIMIZE_LATENCY" | "HIGHER_CONSISTENCY")
            ) || matches!(c.as_u64(), Some(0..=2)),
            "invalid consistency preference",
        )?;
    }
    let empty = Map::new();
    let context = match q.get("context") {
        None | Some(Value::Null) => &empty,
        Some(Value::Object(m)) => m,
        _ => return Err("context must be an object".into()),
    };
    validate_context(&Value::Object(context.clone()))?;
    let allowed = evaluate::Evaluator::new(&model, &stored, &contextual, context).check(&key)?;
    Ok(json!({"allowed":allowed}))
}
fn plan_write(r: &Value, store: &str, model: &ValidatedModel) -> Result<Value> {
    let writes = array(r, "writes")?;
    let deletes = array(r, "deletes")?;
    ensure(
        !(writes.is_empty() && deletes.is_empty()) && writes.len() + deletes.len() <= 100,
        "write must contain 1..100 tuple changes",
    )?;
    let mut statements = Vec::new();
    let mut seen = BTreeSet::new();
    for (del, values) in [(true, deletes), (false, writes)] {
        for v in values {
            let t = tuple(v)?;
            model::object(&t.object)?;
            model::user(&t.user)?;
            ensure(!t.relation.is_empty(), "relation is required")?;
            ensure(
                seen.insert((t.object.clone(), t.relation.clone(), t.user.clone())),
                "duplicate tuple in write request",
            )?;
            let mut s = if del {
                ensure(t.condition.is_none(), "delete must not include a condition")?;
                statement("execute","DELETE FROM wasmfga_tuples WHERE store_id = ? AND object = ? AND relation = ? AND user = ?",vec![store.into(),t.object,t.relation,t.user])
            } else {
                model.tuple(&t)?;
                let body = serde_json::to_string(&t).map_err(|e| e.to_string())?;
                statement("execute","INSERT INTO wasmfga_tuples (store_id, object, relation, user, body) VALUES (?, ?, ?, ?, ?)",vec![store.into(),t.object,t.relation,t.user,body])
            };
            s["expected_changes"] = json!(1);
            statements.push(s);
        }
    }
    Ok(json!({"statements":statements}))
}
pub fn run(input: &[u8]) -> Vec<u8> {
    let result = if input.len() > MAX_INPUT {
        Err("input exceeds 16 MiB".into())
    } else {
        serde_json::from_slice(input)
            .map_err(|e| e.to_string())
            .and_then(process)
    };
    serde_json::to_vec(&result.unwrap_or_else(|error| json!({"error":error})))
        .expect("JSON response serialization")
}

#[cfg(target_arch = "wasm32")]
mod abi {
    use std::cell::RefCell;
    #[derive(Default)]
    struct Buffers {
        input: Vec<u8>,
        output: Vec<u8>,
    }
    thread_local! {static BUFFERS:RefCell<Buffers>=RefCell::new(Buffers::default());}
    #[unsafe(no_mangle)]
    pub extern "C" fn _initialize() {}
    #[unsafe(no_mangle)]
    pub extern "C" fn alloc(size: u32) -> u32 {
        BUFFERS.with_borrow_mut(|b| {
            b.output = Vec::new();
            if size == 0 || size as usize > super::MAX_INPUT {
                b.input = Vec::new();
                return 0;
            }
            b.input = vec![0; size as usize];
            b.input.as_mut_ptr() as u32
        })
    }
    #[unsafe(no_mangle)]
    pub extern "C" fn run() -> u64 {
        BUFFERS.with_borrow_mut(|b| {
            b.output = super::run(&b.input);
            b.input = Vec::new();
            ((b.output.as_ptr() as u64) << 32) | b.output.len() as u64
        })
    }
}

#[cfg(test)]
mod tests {
    use serde_json::{json, Value};
    #[test]
    fn unsupported_inline_subtraction_never_allows() {
        let mut model: Value =
            serde_json::from_str(include_str!("../../tests/fixtures/model.json")).unwrap();
        model["id"] = json!("m");
        model["type_definitions"][3]["metadata"]["relations"]["blocked"]
            ["directly_related_user_types"][0]["condition"] = json!("$expression");
        let owner = json!({"object":"document:1","relation":"owner","user":"user:alice"});
        let blocked = json!({"object":"document:1","relation":"blocked","user":"user:alice","condition":{"name":"$expression","context":{"expression":"['deny'].exists(x, x == 'deny')"}}});
        let result = super::process(
            json!({"operation":"check","store_id":"s","model_id":"m","check":{"tuple_key":{"object":"document:1","relation":"can_view","user":"user:alice"}},"results":[[[model.to_string()]],[[owner.to_string()],[blocked.to_string()]]]}),
        );
        assert!(
            result.is_err(),
            "unsupported exclusion must produce an error, never an allow: {result:?}"
        );
    }
    #[test]
    fn unsupported_named_cel_produces_no_sql() {
        let mut model: Value =
            serde_json::from_str(include_str!("../../tests/fixtures/model.json")).unwrap();
        model["conditions"]["granted"]["expression"] = json!("[level].all(x, x > 0)");
        let output = super::run(
            json!({"operation":"put_model","store_id":"s","model_id":"m","model":model})
                .to_string()
                .as_bytes(),
        );
        let response: Value = serde_json::from_slice(&output).unwrap();
        assert!(response["error"].is_string());
        assert!(response.get("statements").is_none());
    }
    #[test]
    fn large_integer_string_is_not_rounded_through_f64() {
        let mut model: Value =
            serde_json::from_str(include_str!("../../tests/fixtures/model.json")).unwrap();
        model["id"] = json!("m");
        model["conditions"]["granted"]["expression"] = json!("level == 9007199254740993");
        let tuple = json!({"object":"document:1","relation":"conditional","user":"user:alice","condition":{"name":"granted","context":{"required":3}}});
        let response=super::process(json!({"operation":"check","store_id":"s","model_id":"m","check":{"tuple_key":{"object":"document:1","relation":"can_view","user":"user:alice"},"context":{"level":"9007199254740993"}},"results":[[[model.to_string()]],[[tuple.to_string()]]]})).unwrap();
        assert_eq!(response["allowed"], true);
    }
    #[test]
    fn shared_corpus() {
        let mut model: Value =
            serde_json::from_str(include_str!("../../tests/fixtures/model.json")).unwrap();
        model["id"] = json!("m");
        let cases: Vec<Value> =
            serde_json::from_str(include_str!("../../tests/fixtures/cases.json")).unwrap();
        for case in cases {
            let rows: Vec<Value> = case["tuples"]
                .as_array()
                .unwrap()
                .iter()
                .map(|v| json!([v.to_string()]))
                .collect();
            let response = super::process(
                json!({"operation":"check","store_id":"s","model_id":"m","check":case["check"],"results":[[[model.to_string()]],rows]}),
            );
            if case["error"] == true {
                assert!(response.is_err(), "{}", case["name"])
            } else {
                assert_eq!(
                    response.unwrap()["allowed"],
                    case["allowed"],
                    "{}",
                    case["name"]
                )
            }
        }
    }
}
