# Native Go, Rust, and original OpenFGA

Measured 2026-10-21T04:10:25.678Z on Apple M2, darwin/arm64. 5 fresh processes per configuration. These are native library calls; no WASM runtime is in the timed paths.

**Rust is experimental and retains the known contextual-duplicate false allow and CEL gaps.** All measured workloads pass native OpenFGA; this is a performance experiment over that shared domain, not a release conformance claim.

## Checks inside a host-owned SQLite transaction

Median microseconds; lower is faster. The original Go checker is shown both revalidating the model per Check and reusing a compiled immutable model. The latter also reuses its resolver, but never tuple or decision caches.

| Workload | Go snapshot port | Rust snapshot port | Original Go: model per Check | Original Go: model reused |
|---|---:|---:|---:|---:|
| direct-grant | 261.50 | 29.88 | 380.38 | 218.17 |
| empty-deny | 267.67 | 29.54 | 398.46 | 226.88 |
| userset-chain-12 | 427.13 | 47.17 | 763.04 | 546.96 |
| parent-chain-12 | 891.08 | 46.79 | 1,660.29 | 1,442.79 |
| intersection | 273.67 | 30.33 | 401.67 | 236.00 |
| exclusion | 175.58 | 29.38 | 196.88 | 45.79 |
| cel-int | 277.63 | 52.75 | 387.96 | 226.88 |
| snapshot-100 | 452.50 | 105.13 | 384.13 | 221.75 |
| snapshot-1000 | 6,437.54 | 893.63 | 385.13 | 226.75 |
| snapshot-10000 | 479,269.96 | 9,490.25 | 436.67 | 238.25 |

## Native hydrated JSON calls

Identical pre-encoded phase-2 input bytes, without SQL or input encoding; includes output decoding. This keeps the protocol boundary comparable to the earlier WASM guest-only measurements.

| Workload | Go native µs | Rust native µs | Go/Rust |
|---|---:|---:|---:|
| direct-grant | 256.13 | 27.54 | 9.30× |
| empty-deny | 259.67 | 26.96 | 9.63× |
| userset-chain-12 | 430.08 | 43.79 | 9.82× |
| parent-chain-12 | 873.96 | 44.63 | 19.58× |
| intersection | 253.17 | 28.21 | 8.98× |
| exclusion | 163.25 | 27.04 | 6.04× |
| cel-int | 260.29 | 50.33 | 5.17× |
| snapshot-100 | 446.38 | 102.08 | 4.37× |
| snapshot-1000 | 6,406.13 | 887.46 | 7.22× |
| snapshot-10000 | 478,055.71 | 9,606.13 | 49.77× |

## What is being compared

Native library calls in an already-open in-memory SQLite transaction. Includes SQL preparation, reads, stored JSON decoding, validation and evaluation. No outer request/response JSON serialization, WASM, IPC, network or transaction begin/commit costs. Native Go uses a typed Request; native Rust uses a serde_json::Value envelope.

Unmodified upstream commands.NewCheckCommand and graph.NewLocalChecker, bound through a new transaction-only RelationshipTupleReader over wasmfga's two-table schema. Accepts a native protobuf CheckRequest pre-decoded outside timing. Reads relevant edges directly; no complete-store hydration. Not the standalone HTTP/gRPC server or stock OpenFGA SQL backend. Same dispatch depth 25 and breadth 1 as the Go snapshot port. No tuple/decision cache.

The separate upstream-cached variant prepares the immutable model and resolver once per workload/transaction outside timing; preparation is recorded. The other three SQLite variants reload/validate models per Check. This is an application configuration difference, not an isolated language comparison.

Five fresh processes per engine/mode by default, sequential execution, rotating engine order. Median of process medians and median of process p95 values; raw samples and range of medians retained. Small workloads: 500 warmups/5000 measured checks; 1000 tuples: 100/500; 10000 tuples: 10/50. Counts are multiplied by the recorded scale. Every decision verified outside its timed interval. No GC forcing, CPU pinning or profiler.

Rust remains experimental with known conformance gaps including a contextual-duplicate false allow. All measured workloads pass the native OpenFGA oracle; that does not establish general equivalence. Go snapshot hydration still uses quadratic upstream MemoryBackend.Write; direct upstream SQL avoids it. Go and Rust use different SQLite bindings/builds. One in-flight Check at a time; Go may dispatch graph branches using goroutines. One machine, no remote or durable-commit costs. Earlier WASM results were collected in a separate run; cross-run ratios are descriptive, not a simultaneous controlled comparison.

Go: go version go1.27.1 darwin/arm64. Rust: rustc 1.92.0 (ded5c06cf 2025-12-08). Go SQLite: 3.53.4 (modernc.org/sqlite); Rust SQLite: 3.50.2 (rusqlite + bundled SQLite). GOMAXPROCS=8.

The original Go baseline uses upstream's native protobuf CheckRequest, pre-decoded outside timing. The snapshot Go port keeps its inner protobuf-JSON model/query/tuple representation; Rust uses its native Value envelope. Stored model and tuple JSON bytes are identical across hosts. This is a comparison of useful embedding implementations, not an isolated language microbenchmark.

Cold model preparation for the reused-model baseline and first-check times are recorded in the raw samples. They are outside steady-state medians. No startup, library-size or allocation comparison is claimed.

## Verification and reproduction

Native Go, native Rust, and the upstream SQL adapter passed 516 upstream Check assertions, the 21-case shared corpus, model-validation mutations, and 100 Hegel transaction histories each. Rust's native SQLite binding additionally passed shared cases and host commit/rollback/partial-write failure tests. Prepared upstream checks were tested against tentative grant/revoke changes and host commit/rollback. The six known Rust audit disagreements were separately reproduced with the native executable.

The exact benchmark executables pass the ten generated workloads in each measured mode; expected decisions come from unmodified native OpenFGA Server.Check. Every timed result is also verified. The runner checks executable and fixture hashes before and after measurement.

Run `make benchmark-native`. Raw results, medians/p95, process ranges, compiler settings, fixture/executable hashes and all samples are in [native-latest.json](results/native-latest.json); [interactive results](results/native-comparison.html). The native APIs and host transaction contract are described in [native integration](../docs/native.md).
