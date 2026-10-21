use rusqlite::{params, Connection};
use serde::Deserialize;
use serde_json::{json, Value};
use std::{
    io::{self, BufRead, Write},
    time::Instant,
};
use wasmfga_engine::{native, run};

const STORE: &str = "01ARZ3NDEKTSV4RRFFQ69G5FAV";
const ID: &str = "01ARZ3NDEKTSV4RRFFQ69G5FAW";

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Fixture {
    name: String,
    model_body: String,
    check_body: String,
    tuple_bodies: Vec<String>,
    hydrated_input: String,
    allowed: bool,
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.iter().any(|s| s == "--serve") {
        let stdout = io::stdout();
        let mut out = stdout.lock();
        for line in io::stdin().lock().lines() {
            let line = line.unwrap();
            out.write_all(&run(line.as_bytes())).unwrap();
            out.write_all(b"\n").unwrap();
            out.flush().unwrap();
        }
        return;
    }
    let arg = |name: &str| {
        args.windows(2)
            .find(|a| a[0] == name)
            .map(|a| a[1].as_str())
    };
    let mode = arg("--mode").unwrap_or("sqlite");
    assert!(mode == "sqlite" || mode == "json");
    let scale: f64 = arg("--scale").unwrap_or("1").parse().unwrap();
    assert!(scale.is_finite() && scale > 0.0);
    let gate = args.iter().any(|s| s == "--gate");
    let fixtures: Vec<Fixture> =
        serde_json::from_slice(&std::fs::read(arg("--fixtures").unwrap()).unwrap()).unwrap();
    let mut measurements = Vec::new();
    let mut sqlite_version = String::new();
    for w in fixtures {
        let mut db = Connection::open_in_memory().unwrap();
        sqlite_version = db
            .query_row("SELECT sqlite_version()", [], |row| row.get::<_, String>(0))
            .unwrap();
        let mut tx = db.transaction().unwrap();
        native::run(&mut tx, json!({"operation":"schema"})).unwrap();
        tx.execute(
            "INSERT INTO wasmfga_models VALUES (?, ?, ?)",
            params![STORE, ID, w.model_body],
        )
        .unwrap();
        {
            let mut insert = tx
                .prepare("INSERT INTO wasmfga_tuples VALUES (?, ?, ?, ?, ?)")
                .unwrap();
            for body in &w.tuple_bodies {
                let t: Value = serde_json::from_str(body).unwrap();
                insert
                    .execute(params![
                        STORE,
                        t["object"].as_str().unwrap(),
                        t["relation"].as_str().unwrap(),
                        t["user"].as_str().unwrap(),
                        body
                    ])
                    .unwrap();
            }
        }
        let check: Value = serde_json::from_str(&w.check_body).unwrap();
        let request = json!({"operation":"check","store_id":STORE,"model_id":ID,"check":check});
        let mut invoke = || -> bool {
            let response = if mode == "json" {
                serde_json::from_slice(&run(w.hydrated_input.as_bytes())).unwrap()
            } else {
                native::run(&mut tx, request.clone()).unwrap()
            };
            response["allowed"].as_bool().expect("missing decision")
        };
        let verify = |b| assert_eq!(b, w.allowed, "{}", w.name);
        let start = Instant::now();
        let first = invoke();
        let first_us = start.elapsed().as_secs_f64() * 1e6;
        verify(first);
        let (base_iterations, base_warmup) = if w.tuple_bodies.len() >= 10000 {
            (50, 10)
        } else if w.tuple_bodies.len() >= 1000 {
            (500, 100)
        } else {
            (5000, 500)
        };
        let iterations = if gate {
            1
        } else {
            ((f64::from(base_iterations) * scale) as usize).max(10)
        };
        let warmup = if gate {
            0
        } else {
            ((f64::from(base_warmup) * scale) as usize).max(10)
        };
        for _ in 0..warmup {
            verify(invoke())
        }
        let mut durations = Vec::with_capacity(iterations);
        for _ in 0..iterations {
            let start = Instant::now();
            let got = invoke();
            durations.push(start.elapsed().as_secs_f64() * 1e6);
            verify(got)
        }
        durations.sort_by(f64::total_cmp);
        measurements.push(json!({"workload":w.name,"tuples":w.tuple_bodies.len(),"snapshotInputBytes":w.hydrated_input.len(),"iterations":iterations,"warmup":warmup,"firstCheckUs":first_us,"prepareUs":0,"medianUs":durations[iterations/2],"p95Us":durations[(iterations*95/100).min(iterations-1)],"meanUs":durations.iter().sum::<f64>()/iterations as f64}));
        tx.rollback().unwrap();
    }
    std::fs::write(arg("--output").unwrap(),serde_json::to_vec(&json!({"engine":"rust","mode":mode,"measurements":measurements,"sqliteVersion":sqlite_version,"driver":"rusqlite + bundled SQLite"})).unwrap()).unwrap();
    println!("rust {mode}: {} workloads verified", measurements.len());
}
