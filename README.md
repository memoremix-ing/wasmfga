# wasmfga

An embedded WASI port of OpenFGA's **Check** evaluator. Go, Rust, and TypeScript
applications supply an **already-open database transaction**. The guest plans SQL;
the host executes it through that binding. Permission changes, checks, and the
application's own writes can therefore commit or roll back together.

The Go evaluator uses the source in the sibling OpenFGA `lts-1` workspace, including
its graph resolution, model validation, tuple validation, and CEL implementation.
The Rust evaluator in `engine-rust/` independently implements the Check graph and a
conservative scalar CEL subset. It is experimental; see the scope below.

Supported now:

- OpenFGA schema 1.1 authorization model JSON, immutable model IDs, tuple writes/deletes, and Check.
- Usersets, recursive relationships, computed relations, tuple-to-userset,
  wildcards, union, intersection, exclusion, contextual tuples, and CEL conditions.
- SQLite, local libSQL, and the native Turso database engine.
- Go/wazero, Rust/Wasmtime, and TypeScript in Node and browsers using the same WASM artifact.
- Hegel differential testing against an unmodified native OpenFGA `Server.Check`.

This is an initial embedding implementation. Each Check loads the selected model
and a **complete store tuple snapshot**, limited to 10,000 tuples and 16 MiB of
serialized request input. Exceeding either limit returns an error. No partial
snapshot is evaluated. Graph resolution depth is 25. The Go WASM artifact is currently
about 45 MiB before compression; the benchmark report compares its size and
startup with the Rust artifact. This implementation has no HTTP/gRPC service, ListObjects,
ListUsers, Expand, store management, or change-log API. It uses its own two-table
schema rather than OpenFGA's server database schema.

## Build and test

Keep the source-first workspace layout:

```text
track/github.com/openfga/openfga.git/lts-1/
track/github.com/memoremix-ing/wasmfga.git/sts-0/
```

From `wasmfga.git/sts-0`, with Go 1.26.1+ (tested on 1.27.1), Python 3.9+,
Rust 1.92, and Node 24:

```sh
rustup target add wasm32-wasip1
make build                 # Go: dist/wasmfga.wasm; Rust: dist/wasmfga-rust.wasm
make test-go               # Go protocol tests + Go WASM/OpenFGA oracle + Hegel
make test-engines          # oracle/Hegel suites for both WASM implementations
make test-rust             # both guests in Wasmtime + rusqlite transactions
make test-typescript       # both guests on SQLite + libSQL + native Turso
cd sdk/typescript
npm exec playwright install chromium
cd ../..
make test-browser          # both guests + SQLite WASM in Chromium
make test                  # all of the above
make benchmark             # oracle-gated, fresh-process Go/Rust benchmarks
```

`upstream.json` records the source revision tested here. The relative Go module
replacements deliberately build the current sibling checkout, so upstream and
port work can proceed together. CI reconstructs that layout using the recorded
upstream commit. The engine is a separate Go module under OpenFGA's import prefix
because Go's `internal` visibility rules require that prefix to reuse its graph
and validation packages; the host SDK is `github.com/memoremix-ing/wasmfga/sdk/go`.

`scripts/wasm-overlay.py` stages a copy of the pinned `theine-go` dependency in
`dist/` and adjusts its cache-line padding for WASM, where `cpu.CacheLinePad` has
size zero. This is the only dependency portability patch. It never edits the
OpenFGA workspace or the Go module cache. Its original Apache license is retained.

## Independent Rust evaluator and benchmarks

`engine-rust/` builds a Rust `cdylib` for `wasm32-wasip1`, using optimized release,
thin LTO and stripped symbols. The Go engine uses `GOOS=wasip1 GOARCH=wasm`,
`-buildmode=c-shared`, normal compiler optimization and stripped symbols.
Both expose the same `alloc`/`run` ABI and emit the same SQL effects. Every SDK
can load either artifact without a different transaction binding. Models and
tuples written by one engine are readable by the other, including before commit.

The Rust evaluator supports schema 1.1 graph rewrites, usersets, inheritance,
cycles, contextual tuples, and named/inline conditions with scalar CEL expressions.
It passes the same 516 upstream Check/ABAC assertions and 100 Hegel histories.
It also has regression checks for malformed models, computed cycles, protobuf
oneof serialization, cross-engine transactions, large integer strings and
unsupported inline conditions in exclusions.

Its compatibility is narrower than the Go engine: CEL macros, collection
expressions, generic and `any` parameters, and unrecognized model fields are
rejected. Numeric strings must spell signed 64-bit integers; double strings are
further restricted to exactly representable integers within ±2^53. Decimal or
scientific numeric strings remain unsupported (JSON numeric values are supported).
The scalar type checker and structural model validator are independent ports,
not the upstream Go validators. Passing these tests does not establish full
OpenFGA/CEL equivalence or production readiness. Keep the Go implementation as
the compatibility baseline while extending and testing Rust.

Run `make benchmark` to validate **both exact artifacts** against native OpenFGA
before collecting timings. It measures cold V8 compilation/initialization,
hydrated guest calls, and complete checks inside an existing local SQLite
transaction. Ten workloads cover direct grants/denies, recursion, intersection,
exclusion, conditions, and 100/1,000/10,000-tuple snapshots. Five fresh processes
per implementation and mode alternate execution order. Every measured decision
is checked. Raw results include artifact hashes, toolchain/hardware details,
warmups, input sizes, per-process medians/p95 and linear-memory high-water marks.

See [benchmark results](benchmarks/RESULTS.md),
[raw samples](benchmarks/results/latest.json), and
[interactive comparison](benchmarks/results/comparison.html).
These measure the two implemented ports, including their different runtime,
telemetry and validation machinery; they are not universal language benchmarks.

## Transaction contract

The binding only supports query and execute. The SDKs and guest never begin,
commit, roll back, open a database, or switch connections. The host must:

1. Begin a transaction on the database containing both authorization and application data.
2. Pass the same transaction to each wasmfga operation and its application logic.
3. Use that transaction exclusively while an operation runs.
4. Commit on success; **roll back on any error**, including validation or SQL errors.

A batch may have executed some statements before a subsequent statement fails.
The SDK cannot roll it back because transaction ownership belongs to the caller.
Missing deletes and duplicate inserts are errors. Model IDs are immutable and
must be selected explicitly. Tuples are scoped to a store, not a particular model,
matching OpenFGA's storage model. Request-local data is discarded after every
operation; no authorization cache survives between calls or transactions.

Use a write/serializable transaction when a check must protect a later write.
For remote libSQL/Turso, retain the same interactive transaction object and obey
the database's transaction lifetime limits. Network Turso Cloud is supported by
the binding shape but has not been tested against a live account. `dialect:
"turso"` uses the same SQL as `"sqlite"`; PostgreSQL is explicitly rejected for now.

## TypeScript (Node or browser)

Build `sdk/typescript` with `npm ci && npm run build`. Browser bundlers can import
the same module; it has no Node runtime imports. Load the artifact with
`await WasmFGA.create(await (await fetch(wasmURL)).arrayBuffer())` in a browser.
Using a Worker keeps synchronous WASM evaluation off the UI thread.

```ts
import { WasmFGA, libsqlTransaction } from "./sdk/typescript/dist/index.js";

// Node: wasmBytes from fs.readFile; browser: ArrayBuffer from fetch.
const fga = await WasmFGA.create(wasmBytes);
const tx = await database.transaction("write"); // host-owned @libsql/client transaction
try {
  const binding = libsqlTransaction(tx);
  const result = await fga.run(binding, {
    operation: "check", store_id: storeID, model_id: modelID,
    check: { tuple_key: { object: "document:1", relation: "can_view", user: "user:alice" } },
  });
  if (!result.allowed) throw new Error("access denied");
  await tx.execute({ sql: "UPDATE documents SET title = ? WHERE id = ?", args: [title, 1] });
  await tx.commit();
} catch (error) {
  await tx.rollback();
  throw error;
}
```

`preparedTransaction(connection)` supports `node:sqlite` and
`@tursodatabase/database` connections on which the **caller has already issued
BEGIN**. Custom browser/ORM bindings implement `Transaction.query` (text row
arrays) and `Transaction.execute` (affected row count). See the integration tests
for full schema setup, model writes, tuple writes, commit, and rollback examples.

## Go and Rust

Go: `wasmfga.New(ctx, wasmBytes)`, then `engine.Do(ctx,
wasmfga.SQLTransaction{Tx: tx}, wasmfga.Request{...})`. `tx` is a caller-created
`*sql.Tx`. Defer `tx.Rollback()` before the first operation; explicitly commit
only after application logic succeeds. Engine calls are serialized. Close the
engine when finished; cancellation closes the WASM instance, so recreate it after
a canceled/trapped call.

Rust: `wasmfga::Engine::new(&wasm_bytes)?`, then
`engine.run(&mut transaction, serde_json::json!({...}))?`.
`rusqlite::Transaction` implements the binding trait. Its borrow prevents
concurrent use, and its default drop behavior rolls back if application logic
returns early. Applications can implement the same trait for other hosts.

## Correctness coverage

The shared 21-case corpus is verified against native OpenFGA and exercised by all
three SDKs, including the browser. It covers positive and negative graph
decisions, cycles with/without a proof, conditions with missing context, and
invalid requests. Each host additionally checks that authorization grants and
application writes become visible before commit and persist/disappear together.

`tests/upstream_test.go` additionally runs all **516 Check assertions** in the
sibling upstream's `consolidated_1_1_tests.yaml` and `abac_tests.yaml` against both
native OpenFGA and the SQL-backed WASM, including multi-stage model evolution.

`tests/hegel_test.go` uses [Hegel](https://hegel.dev/) (`hegel.dev/go/hegel`, pinned
in `go.mod`). It generates nested model rewrites and 100 histories of up to 35
grant/revoke/check/commit/rollback steps. An independent in-memory state model
tracks tentative and committed tuples plus application writes. Native OpenFGA
evaluates that model state; the WASM evaluates the SQL state. Every step compares
decisions/error presence and application row counts. Hegel shrinks failures and
prints the model draws and operation trace. Hegel retains its normal failure
replay database outside CI (persistence is disabled by default in CI).
`go test ./tests -run TestHegel -count=1` reruns it.

See [docs/protocol.md](docs/protocol.md) for the host ABI and PostgreSQL extension path.
