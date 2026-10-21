# Mechanically driving correctness

The target is observable refinement of a pinned OpenFGA implementation, plus
the embedding's own transaction contract. Test counts and branch coverage are
supporting measurements, not the acceptance criterion.

## Contract

Define a versioned supported-input domain and normalize outcomes into Allow,
Deny, or Error(category), retaining error phase (decode, model validation, tuple
validation, condition conversion, condition evaluation, resolution, host SQL).
Timeouts, traps, exhausted budgets and crashes must be distinguished from Deny.
They never count as successful checks or as passing comparisons.

Two independent obligations matter:

- Safety: whenever Rust returns Allow, the configured oracle must return Allow.
  Oracle errors also fail this implication. Unsupported inputs must not disappear
  from an exclusion or otherwise lead to a permission grant.
- Compatibility: throughout the advertised domain, model/tuple acceptance and
  Check outcomes must match, including normalized error categories. Returning an
  error for every request is safe under the first implication but incompatible.

For write/transaction traces, compare logical committed state and observations,
not physical SQL layouts. Model the host's rollback after any guest/SQL error:
the existing protocol permits partial statement execution before rollback. The
guest must never own commit, rollback, begin, or an alternate database connection.
Boundaries (snapshot/input/dispatch limits, consistency and CEL configuration)
must be explicit and aligned with the oracle where applicable.

Compare semantic outcomes and state, not incidental error text or SQL query
order. Specify any permitted diagnostic nondeterminism in upstream parallel
evaluation explicitly; do not hide disagreements by collapsing all failures.

## Immediate executable gate

Implemented in this change:

```sh
make conformance
```

This builds both guests and runs all `./tests` tests with the
`conformance_audit` tag. There are no known-failure exemptions. It continues to
Rust after Go, returns nonzero on disagreement, writes raw Go test JSON streams
to `dist/conformance/{go,rust}.jsonl`, and writes a report containing artifact
hashes, the requested upstream manifest, command/filter, failed test names and
artifact stability checks. A zero-matching-test run cannot pass.

A focused diagnostic reuses existing artifacts:

```sh
python3 scripts/conformance.py --run '^TestConformanceAudit'
```

The report labels this as filtered coverage. This invocation passes for Go and
fails for Rust on the six existing audit cases. The gate deliberately stays red
until compatibility improves. The original default test and benchmark gates do
not include all open gaps; they must not be interpreted as release conformance.
This command is not yet wired into CI or release publishing. The manifest records
the requested upstream revision; local source-first builds still consume the
current sibling source. CI must enforce the source pin, and stronger provenance
should also record the actual source-tree digest and oracle configuration.

## Systematic generation

The following are the next implementation stages, not completed capabilities.

Use one portable JSON scenario format: model(s), persisted tuples, ordered
contextual tuples, request context, operations, transaction boundaries and fault
schedule. Native OpenFGA, Go WASM and Rust WASM consume equivalent scenarios.
The host adapters additionally check transaction observations and committed state.
Keep the generator independent of Rust's validator; classify validity using the
oracle. Otherwise Rust's own acceptance bugs would silently limit test inputs.
The current oracle helper populates its memory datastore directly; that is not
an oracle for Write acceptance. Model/write generators must exercise upstream
`Server.WriteAuthorizationModel` and `Server.Write` validation paths (mapping
generated model IDs as needed), not infer validity from successful datastore setup.
Record the first divergent stage: decoding, model acceptance, context conversion,
logical reader results, condition outcome, graph outcome, or committed state.

First enumerate every instance of narrowly defined finite families: reader kind
(direct/userset/wildcard/tuple-to-userset), persisted/contextual placement,
duplicate keys and ordering, condition outcomes (true/false/error), and rewrite
context (direct/union/intersection/exclusion). Enumerate all requests for each
tiny instance and retain the exact grammar and bounds in reports. A bounded
family is not an exhaustive claim about arbitrary authorization models. Never
deduplicate contextual tuples or sort their order during scenario normalization.

Then use Hegel for larger type graphs and histories with generated conditions,
model evolution, malformed inputs, grants/revokes, commit/rollback and injected
host failures. Shrinking must preserve the disagreement, not just validity.
Every failure becomes a portable scenario with oracle source/configuration and
guest hashes, expected normalized outcome, and replay command. Replay all saved
counterexamples before generating new ones. Preserve Hegel's replay data too;
the portable scenario avoids depending solely on one library version's draw stream.

CEL needs a separate differential pipeline for model compilation/type checking,
context conversion, and evaluation. Start with the OpenFGA-relevant portion of
the official cel-spec suite and its custom IP extension; generate typed ASTs and
near-valid mutations with numeric, Unicode, time and error boundary values.

## Check the checking machinery

Add metamorphic properties with explicit preconditions: opaque identifier
renaming preserves decisions, store isolation holds, serialization round-trips
preserve meaning, and rollback restores observations. Do not assume that adding
a grant can only add permissions: exclusions invalidate that property. Likewise,
contextual tuple order is not universally interchangeable with persisted order.

Deliberately seed faults (drop a condition, ignore subtraction, incorrectly
deduplicate contextual edges, reuse stale transaction data, round large integers)
and require the suite to detect them. A green suite that cannot detect these
faults needs a stronger generator or comparison rule. Track uncovered semantic
combinations and surviving mutants, not just total executions.

## Smaller proof obligations

Extract a small pure semantic core from JSON/SQL/WASI. Specify outcome combination,
cycle propagation and depth counting, exact read-operation precedence, and
parameter conversion. Preserve the behaviors observed in the pinned oracle;
ordinary Boolean equations do not completely specify errors and cycles.

Use bounded model checking (for example Kani) on suitable Rust kernels: explore
all bounded outcome combinations, reader overlap cases and integer boundaries;
state the bounds, check unwinding completeness and avoid assumptions that exclude
the interesting cases. Larger loops/library integrations may require refactoring.
A proof about a kernel and a written specification does not prove the specification
matches OpenFGA, or that WASI/host integration is correct. Differential tests and
runtime integration tests remain necessary. An unbounded equivalence claim would
require stronger inductive proofs and a formally related OpenFGA semantics.

Reduce duplicated semantics where practical: generate wire bindings from a
single schema, and investigate an upstream-validated portable model/checked-CEL
format compiled before deployment. An offline Go compiler can reduce what the
Rust runtime must reimplement without requiring Go in the browser. This needs
an explicit versioned format and validation/trust contract, and does not by
itself establish compatible CEL execution or graph resolution.

## Operational loop

For every candidate change: replay counterexamples, run fixed bounded enumeration
and deterministic generation, compare exact optimized WASM artifacts, shrink new
disagreements, repair, and repeat. Scheduled longer runs expand bounds/seeds;
upstream upgrades run old/new oracles on the same scenarios to distinguish upstream
behavior changes from port regressions.

Only advertise a feature after its semantic obligations and supported domain are
in the gate. Reject unsupported features explicitly. No silent skips count as
passes; resource exhaustion is an incomplete comparison. Gate benchmarks/releases
on correctness for the identical artifact hashes. Add browser/Wasmtime/wazero,
SQL adapters, remote interactive transactions, concurrency and cancellation as
separate integration obligations rather than conflating them with graph proofs.

Primary references:

- Hegel: https://hegel.dev/
- CEL specification and conformance data: https://github.com/cel-expr/cel-spec
- Kani usage: https://model-checking.github.io/kani/usage.html
- Kani proof bounds: https://model-checking.github.io/kani/tutorial-loop-unwinding.html
