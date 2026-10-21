# Native libraries and benchmarks

The same evaluator sources can be linked directly into native applications.
Neither native path loads a WASM runtime. Both borrow an already-open transaction;
the application controls begin/commit/rollback and must roll back on any error.
A multi-statement batch may have made tentative changes before a later error.

## Go

Import `github.com/memoremix-ing/wasmfga/sdk/go/native`:

The source-first Go replacements in this repository are required. A consuming
application must also replace `github.com/openfga/openfga/wasmfga` with this
workspace's `engine/` and `github.com/openfga/openfga` with the sibling `lts-1`
workspace: dependency modules' replace directives are not inherited. The engine's
OpenFGA import prefix permits reuse of upstream internal packages; it is not a
claim that an OpenFGA-owned wasmfga module has been published.

```go
tx, err := db.BeginTx(ctx, nil) // application-owned
if err != nil { return err }
defer tx.Rollback()
result, err := native.Do(ctx, native.SQLTransaction{Tx: tx}, native.Request{
    Operation: "check", StoreID: storeID, ModelID: modelID, Check: checkJSON,
})
if err != nil { return err }
if result.Allowed == nil || !*result.Allowed { return errNotAuthorized }
// Application writes use this same tx.
return tx.Commit()
```

`Transaction` also supports custom hosts. Its only methods are Query and Execute.
The native API calls `engine.Process` directly rather than encoding the outer
request to JSON. Model, tuple and Check bodies still use protobuf JSON internally.
Snapshot, batch, contextual tuple and graph-depth limits remain; the 16 MiB
serialized-input limit applies to `engine.Run`, not the structured native API.
Native-only database/sql adapters are excluded from Go WASI builds.

## Rust

Link the `wasmfga-engine` crate in `engine-rust/`; enable feature `sqlite` for its
rusqlite transaction implementation. No Wasmtime dependency is needed.

```rust
use wasmfga_engine::native;
use serde_json::json;

let mut tx = connection.transaction()?; // application-owned
let result = native::run(&mut tx, json!({
    "operation": "check", "store_id": store_id, "model_id": model_id,
    "check": {"tuple_key": {"object":"document:1", "relation":"can_view", "user":"user:alice"}}
}));
match result {
    Ok(value) if value["allowed"] == true => {
        // Application writes use this same tx.
        tx.commit()?;
    }
    _ => tx.rollback()?,
}
```

Implement `native::Transaction` for another host. `process_ref` exposes borrowed
structured evaluation and `run` exposes the serialized protocol. Only the latter
enforces the serialized-input cap. The Rust evaluator remains experimental:
the known CEL restrictions and contextual-duplicate false allow also exist natively.

## Original Go baseline

`engine/upstream` calls unmodified upstream `commands.NewCheckCommand` and
`graph.NewLocalChecker`, with a new `RelationshipTupleReader` that queries the
supplied transaction directly over wasmfga's schema. This avoids complete-store
hydration. It implements the reader operations needed by Check; pagination is
explicitly unsupported. The upstream source is not changed.

`upstream.CheckRequest` reloads and validates the model each time.
`upstream.Prepare` reads/validates the immutable model once and retains a resolver;
`Prepared.CheckRequest` then accepts native upstream protobuf requests. Prepared
objects are bound to their transaction/store/model and must not outlive that
transaction. Close releases only the resolver. No tuple or decision cache survives
between checks; tentative grants/revokes remain visible.

This measures the original Go library checker with a transaction adapter, not the
standalone HTTP/gRPC service or the stock OpenFGA SQL backend. Compiled-model reuse
is reported separately from per-Check model validation.

## Reproduce

```sh
make test-native
make benchmark-native
```

The native build does not require WASM artifacts. The existing mixed-WASM test
is skipped if those optional artifacts are absent; native checks and transaction
tests still run. The benchmark gate checks ten shared workloads, including the
10,000-tuple snapshot, against native OpenFGA Server.Check and every measured
executable/mode. It does not waive or resolve the broader Rust conformance gaps.

Five fresh processes per engine/mode run sequentially, rotating engine order.
Small cases use 500 warmups/5,000 measurements; 1,000 tuples use 100/500;
10,000 tuples use 10/50. Every returned decision is verified outside its timer.
The report includes medians/p95, process ranges, toolchains, SQLite versions,
binary and fixture hashes. Rust release uses opt-level 3 and thin LTO; native Go
uses normal optimization with stripped symbols. No profiler or concurrent tests
run during measurement.

The SQLite mode uses native library calls, existing host transactions and the
language's SQLite adapter. The JSON mode feeds identical pre-encoded hydrated
inputs to native protocol functions, excluding SQL/input encoding. Drivers differ
between Go and Rust. Go's snapshot adapter retains quadratic MemoryBackend.Write
hydration; the upstream direct-reader baseline does not incur that cost.

Results: [native comparison](../benchmarks/NATIVE_RESULTS.md),
[raw samples](../benchmarks/results/native-latest.json), and
[interactive comparison](../benchmarks/results/native-comparison.html).

Source provenance uses the report's jj change and verified upstream revision.
The measured Go executable also contains Go's automatically collected enclosing
Git metadata; that is not the wasmfga revision. Subsequent native builds disable
automatic VCS stamping with `-buildvcs=false`. The archived measurements and their
original executable hashes/build settings are retained unchanged.
