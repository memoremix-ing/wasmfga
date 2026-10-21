# Go WASM versus Rust WASM

Measured 2026-10-21T03:04:42.383Z on Apple M2, darwin/arm64, v24.22.0, V8 13.6.233.17-node.57. 5 fresh processes per artifact and mode. Both are optimized WASI reactors; Go is stripped and Rust uses release + thin LTO.

| Artifact/startup | Go | Rust |
|---|---:|---:|
| WASM size (MiB) | 44.55 | 3.42 |
| gzip size (MiB) | 8.71 | 0.92 |
| V8 compile (ms) | 29.49 | 3.03 |
| Instantiate + initialize (ms) | 120.97 | 0.33 |
| First SQLite Check after schema setup (ms) | 45.05 | 14.09 |
| Max observed linear memory (MiB) | 65.50 | 10.81 |

## Complete Check inside a host SQLite transaction

Complete two-call Check inside an existing SQLite in-memory transaction; includes SQL preparation/reads and host JSON encoding/decoding. Excludes fixture setup and transaction begin/commit/rollback.

| Workload | Go median (µs) | Rust median (µs) | Go/Rust | Go p95 (µs) | Rust p95 (µs) |
|---|---:|---:|---:|---:|---:|
| direct-grant | 1,266.75 | 67.25 | 18.84× | 2,766.25 | 93.04 |
| empty-deny | 1,196.75 | 57.58 | 20.78× | 2,533.67 | 67.00 |
| userset-chain-12 | 1,768.17 | 81.58 | 21.67× | 3,665.25 | 97.21 |
| parent-chain-12 | 3,463.87 | 81.33 | 42.59× | 7,917.71 | 93.67 |
| intersection | 1,220.04 | 55.96 | 21.80× | 2,591.17 | 64.21 |
| exclusion | 975.79 | 54.37 | 17.95× | 2,317.54 | 59.42 |
| cel-int | 1,253.71 | 84.54 | 14.83× | 2,648.50 | 95.46 |
| snapshot-100 | 2,110.25 | 200.38 | 10.53× | 4,465.46 | 220.08 |
| snapshot-1000 | 24,914.62 | 1,724.75 | 14.45× | 31,781.50 | 1,829.21 |
| snapshot-10000 | 1,696,545.62 | 19,048.38 | 89.07× | 1,751,263.67 | 21,489.46 |

## Hydrated guest invocation

One hydrated phase-2 ABI call; pre-encoded input. Includes guest JSON parsing, model/CEL validation, tuple validation/indexing and resolution; output decoding included. Excludes host SQL and input encoding.

| Workload | Go median (µs) | Rust median (µs) | Go/Rust | Go p95 (µs) | Rust p95 (µs) |
|---|---:|---:|---:|---:|---:|
| direct-grant | 1,222.21 | 52.46 | 23.30× | 2,575.33 | 109.71 |
| empty-deny | 1,170.08 | 39.71 | 29.47× | 2,492.33 | 50.13 |
| userset-chain-12 | 1,719.79 | 57.33 | 30.00× | 3,637.37 | 65.62 |
| parent-chain-12 | 3,407.08 | 57.75 | 59.00× | 7,953.33 | 64.96 |
| intersection | 1,186.37 | 37.79 | 31.39× | 2,528.00 | 45.29 |
| exclusion | 943.13 | 36.42 | 25.90× | 2,302.54 | 42.67 |
| cel-int | 1,216.42 | 64.71 | 18.80× | 2,581.83 | 70.88 |
| snapshot-100 | 2,024.25 | 144.17 | 14.04× | 4,303.71 | 155.92 |
| snapshot-1000 | 24,434.04 | 1,328.58 | 18.39× | 31,285.42 | 1,372.79 |
| snapshot-10000 | 1,698,294.96 | 15,579.29 | 109.01× | 1,787,662.83 | 16,062.71 |

## Snapshot adapter cost

Source inspection: Go hydrates each snapshot through upstream MemoryBackend.Write, whose insertion loop scans previously accumulated records for every tuple (pkg/storage/memory/memory.go:391). It also creates ULIDs and change records. Rust builds an indexed read view. The large-snapshot comparison therefore includes a quadratic Go adapter cost; it is not an isolated graph-algorithm or language speed comparison. No CPU profile was taken to assign percentages to individual costs.

An indexed read-only relationship reader is the next Go optimization to test; it was not applied in this comparison.

## Scope and reproducibility

Median of per-process medians across fresh processes. p95 is median of process p95 values. Process order alternates. Every response is checked. Warmups and iteration counts are recorded. No persistent guest authorization caches.

Fresh-process WebAssembly.compile and instantiate+_initialize; excludes artifact I/O, process launch and network. V8 may defer optimization to warmup. First SQLite Check is reported separately after schema setup, before any model/check warmup.

Max observed linear memory across all workloads, and peak RSS of a complete worker process (includes Node/V8/SQLite/JIT). Neither is per-check allocation.

Rust is an experimental independent evaluator with a conservative scalar CEL subset and restricted numeric-string conversion; passing tested corpora is not full OpenFGA or CEL conformance. Results compare these two ports, including differing validation/telemetry/runtime designs, rather than Go versus Rust universally. One local machine; no remote database/network or durable commit costs measured.

Correctness gate: 516 upstream assertions, 21 shared cases, 100 Hegel histories per artifact, transaction failure/isolation checks, all generated benchmark workloads. Native OpenFGA supplies the expected decisions for every benchmark workload, including the 10,000-tuple snapshot.

Run `make benchmark`. Raw samples, hashes, compiler versions, input sizes, warmups and iteration counts are in [latest.json](results/latest.json). The SQL protocol and complete snapshot policy are identical, and every timed result is verified. Benchmarks run sequentially; transaction creation/commit and network/disk latency are excluded.

The Rust implementation supports scalar CEL expressions, including named and inline conditions, timestamps, duration conversion, regex/string predicates, and IP/CIDR matching. CEL collections, generic/any parameters and macros are rejected. Its model validation is an independently implemented subset; the Go engine retains upstream's validator and runtime. Passing these suites does not establish unrestricted API/model/CEL equivalence or production readiness for the Rust prototype.
