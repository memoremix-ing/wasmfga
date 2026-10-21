# WASM and transaction protocol (v0)

Build target: Go `wasip1/wasm`, `-buildmode=c-shared`, WASI preview 1 reactor.
Initialize `_initialize` once, providing `argv[0] = "wasmfga"`, random and clock
services, and standard streams. No filesystem preopens or network imports are
needed. All text is UTF-8. The WASI runtime and SQL binding are trusted host code.

Exports:

- `alloc(size: i32) -> i32`: allocate a 1..16 MiB input buffer; return its pointer
  (zero on rejected size). Release the previous output and replace the input.
- `run() -> i64`: consume the allocated input and return
  `(u64(output_pointer) << 32) | u64(output_byte_length)`.

The host writes a JSON request into the input, invokes `run`, then copies/decodes
the output before another call. Memory may grow, so reacquire views after calls.
One input/output pair exists per instance. Calls into one reactor must be
serialized; different transactions can share it because it retains no semantic
state between requests. A trap invalidates the operation: roll back the host
transaction and discard the reactor. There is no callback/reentrant guest stack.

Requests use these operations:

- `schema`: emit idempotent table creation statements, executed in the supplied transaction.
- `put_model`: `store_id`, `model_id`, `model` (protobuf JSON AuthorizationModel).
  Validate before emitting an immutable insert. The selected ID is assigned to
  the canonical serialized model.
- `write`: `store_id`, `model_id`, `writes`/`deletes` (arrays of protobuf JSON TupleKey).
  At most 100 changes. Deletes exclude condition data. Duplicate entries within
  one request are rejected. A tuple cannot be both deleted and written in one request.
- `check`: `store_id`, `model_id`, `check` (protobuf JSON CheckRequest body).
  The host envelope selects the store/model; embedded store/model IDs are not used.

Each SQL statement has `kind: "query" | "execute"`, `sql`, and `params: string[]`.
Every parameter is bound, including store IDs, model IDs, and tuple JSON. All
query columns are TEXT and all rows are `string[]`. An execute statement may
require `expected_changes: 1`; the host must check the affected row count.

Check/write have at most two calls:

1. The initial request has no `results`. The response has `needs_results: true`
   and query statements. Execute them sequentially on the supplied transaction.
2. Resubmit the identical request plus `results: string[][][]`, one row set for
   each query in order. The guest validates the model, evaluates a check or plans
   tuple writes, and returns either `allowed: boolean` or execute statements.

Schema/model writes need only one call. Any `error` string is terminal and must
never be treated as a deny/allow decision. Host query/execute errors are also
terminal. The caller rolls back; the guest has no transaction lifecycle authority.
SDKs discard externally supplied `results` and fetch through their binding.

The host chooses transaction isolation. Snapshot hydration is per operation, so
it sees earlier writes in the same transaction. No tuple/model/decision cache
crosses operations. A check uses higher consistency in the upstream command and
an ephemeral in-memory relationship reader. The memory datastore's Write method
hydrates that reader only; it never opens or commits a database transaction.

## PostgreSQL next

The SQL effect protocol and host binding are runtime-agnostic. Add a dialect
implementation for PostgreSQL placeholders (`$1`, `$2`, …), migrations and indexes,
then adapters borrowing an existing pgx/sqlx/TypeScript transaction. Keep model
IDs immutable and tuple keys unique. Verify affected-row behavior, error-to-host
mapping, repeatable/serializable snapshots, aborted transaction behavior, and
concurrent revocations with the same Hegel history suite before enabling it.

For larger stores, replace full snapshots with bounded query continuations or an
async-capable relationship reader. Preserve transaction identity across every
continuation and never silently truncate. Native OpenFGA remains the oracle;
performance work must run the differential corpus and generated histories.
