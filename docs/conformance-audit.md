# Rust conformance audit

This audits the artifacts used by the Go/Rust benchmark. It does not change either
evaluator or invalidate the recorded timings for their tested workloads. It does
demonstrate that the passing benchmark gate is narrower than OpenFGA equivalence.

## Reproduced gaps

`tests/conformance_audit_test.go` contains six oracle comparisons. All six pass
with the Go WASM guest and fail with the current Rust WASM guest:

```sh
go test -tags conformance_audit ./tests -run '^TestConformanceAudit' -count=1 -v
WASMFGA_WASM="$PWD/dist/wasmfga-rust.wasm" go test -tags conformance_audit ./tests -run '^TestConformanceAudit' -count=1 -v
```

The Rust invocation is expected to exit unsuccessfully until these gaps are fixed.
The build tag keeps this diagnostic suite separate from the currently passing
default suite; move each regression into the default gate when fixed. There are
no evaluator fixes in this audit change.

The highest priority is **an incorrect allow**, not unsupported CEL syntax:

- A stored owner grant allows `user:alice` to view `document:1` unless blocked.
- `group:1#member` contains Alice. Two request-local `blocked` edges point to
  that same userset. Their supported `level >= required` conditions use required
  values 2 and 0, respectively, with request level 1.
- OpenFGA considers both userset edges: the second blocks Alice, so Check denies.
- Rust deduplicates by object/relation/user before evaluation, keeping only the
  first, false edge. Its exclusion therefore allows Alice.

Upstream `CombinedTupleReader.ReadUsersetTuples` combines contextual and persisted
iterators, retaining duplicates; direct `ReadUserTuple` has different precedence.
The Rust read view must reproduce the semantics of each reader operation. Merely
removing all deduplication risks changing direct-tuple behavior in the other direction.

Four valid model conditions are accepted by native OpenFGA and the Go guest but
rejected by Rust: `'a' + 'b' == 'ab'`, `int('1') == 1`,
`duration('1s') + duration('1s') == duration('2s')`, and
`[1, 2].exists(x, x == 1)`. The sixth comparison uses the supported condition
`level >= required`: native OpenFGA accepts integer context string `"1e0"`, while
Rust rejects it. The tests validate every example model with upstream
`typesystem.NewAndValidate` before comparing guest behavior.

## CEL support boundary

The port uses the Rust `cel` interpreter behind a hand-written conservative type
checker. These are restrictions of this port, not an inventory of everything the
underlying crate can do.

Supported parameter types: bool, int, uint, double, string, timestamp, duration,
and OpenFGA IP addresses. Supported operations include Boolean logic, same-type
comparisons, numeric arithmetic, ternaries, timestamp/duration conversion,
IP/CIDR matching, and startsWith/endsWith/contains/matches string predicates.

Rejected features include any/list/map parameters and generic types; collection
literals, selection/indexing/membership and macros; numeric/string/dynamic
conversions and size/type functions; string concatenation; timestamp/duration
arithmetic and time accessors. Null and bytes literals are also rejected.
Numeric strings are restricted to signed-64-bit integer spellings, with a further
integer-range restriction for double strings. Even supported operations need
boundary tests for overflow, errors and short-circuiting, Unicode/regex behavior,
timestamp/duration parsing, and IP normalization. Runtime CEL cost accounting and
cancellation also need an explicit portable contract.

## What current coverage establishes

The default suites compare 516 upstream Check assertions from two YAML corpora,
21 shared cases, 12 model-validation mutations, and 100 Hegel transaction histories.
The upstream runner does not reproduce upstream's additional pass converting
persisted fixture tuples into contextual tuples. It compares error presence,
not normalized error categories.

Hegel currently varies nested rewrites in one fixture model and histories over a
fixed pool of 17 tuples. It does not generate arbitrary type graphs, condition
programs, malformed models, or overlapping conditional contextual tuples. The
duplicate-edge counterexample is outside that generator's domain.

Both ports currently expose Check and their embedding protocol, not the complete
OpenFGA server API. The common snapshot/size/depth limits remain a separate scope
boundary from Rust-vs-Go conformance.

## Recommended gates, in order

1. Fix the false allow and expand its regression matrix: direct/userset/wildcard/
   tuple-to-userset reads; contextual/contextual and contextual/persisted overlaps;
   both condition orders; union, intersection and exclusion. Require equality of
   allow/deny/error; treat every Rust allow against oracle deny/error as critical.
2. Extend Hegel to generate type graphs, valid and invalid models, tuple/context
   combinations, model evolution, and transaction histories together. Shrink and
   retain every disagreement as a normal regression. Add longer scheduled runs.
3. Run the upstream contextual variants and relevant graph/validation matrices.
   Compare normalized error categories as well as decisions. Pin the oracle source
   and configuration; test candidate upstream upgrades before changing the pin.
4. Differential-test CEL model compilation, parameter conversion and execution
   separately. Adapt the official cel-spec conformance corpus to OpenFGA's actual
   environment, including its IP extension. Expand language support only behind
   these tests; scalar-only policies still require semantic testing.
5. Extend transaction/runtime tests to multi-connection contention, statement
   failure injection, cancellation and resource limits, model changes, concurrent
   grants/revokes, and supported browser/runtime versions. Local libSQL/Turso tests
   do not substitute for testing a real remote interactive transaction.

A practical release criterion is zero known semantic disagreements within an
explicitly advertised input domain, all exposed operations covered by the oracle,
replayable/shrunk failures, and documented resource/transaction behavior. Passing
more random histories alone is not a conformance claim.
