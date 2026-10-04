# Compatibility contract

`aiusage-core` is an importable Go module. Applications own configuration,
scheduling, process lifetime, credentials, and presentation. No aiusage
executable or TUI dependency is required.

## Go and platforms

The minimum Go version is declared in [go.mod](go.mod), currently 1.25.13.
[.go-version](.go-version) pins the current integration toolchain, 1.26.6,
matching UAM. These are deliberate test versions, not floating aliases for the
latest Go release. Normal builds use `CGO_ENABLED=0`. Only the race-detector
job enables CGO and requires a C compiler.

Linux and macOS are the supported operating systems. CI defines this coverage:

| Target | Minimum Go | Current integration Go |
| --- | --- | --- |
| Linux amd64 | Native fixture tests and build | Native fixture tests, build, race detector, vulnerability scan |
| Linux arm64 | Not tested separately | Cross-build |
| macOS arm64 | Not tested separately | Native fixture tests and cross-build |
| macOS amd64 | Not tested separately | Cross-build |

Native tests use `ubuntu-24.04` and `macos-15` runners. Cross-builds verify that
packages compile for the target; they do not execute SQLite, source discovery,
or filesystem behavior on that target. A passing native fixture suite verifies
the included fixtures, not every installed harness version. Windows and other
Go versions have no CI support commitment in this policy.

## Public packages and releases

The supported imports are `model`, `adapter`, the concrete adapter packages,
`adapter/all`, `collect`, `store`, and `pricing`. Internal packages are not
extension points. Implementers of `adapter.Adapter` can add capabilities via
optional interfaces; new capabilities must not add required methods to that
interface.

The released `v0.1.0` API is the compatibility baseline for this work. Existing
imports, exported signatures, struct fields, required interface methods and
documented default behavior must remain usable. New behavior requiring a
different accounting contract is opt-in. Correctness fixes restore the stated
accounting semantics; they do not promise preservation of incorrect totals.
Changing the module
import path changes Go type identity: do not mix model or store types from the
original `aiusage` module with this module's types.

Consumers should pin a released module version. The store's SQL schema is
owned by the library, not a stable API for application writes. `store.Open`
applies forward migrations to the output ledger; opening a migrated ledger
with older library versions is not guaranteed. Back up the ledger before an
upgrade that changes its schema. Do not point the store at a harness database
or another application's database.

`scripts/check-api.sh` uses the Go project's pinned `gorelease` tool against
`v0.1.0`, with proposed version `v1.0.0`. The wrapper explicitly rejects the
tool's incompatible-change report, because gorelease alone permits breaks
when its baseline is v0. This is a compatibility check, not a release
command or a claim of completed production certification. The script includes
uncommitted repository files and does not alter the working tree. Behavioral
compatibility is covered separately by regression tests. When preparing later
releases, also compare against the latest released API by passing its version
and the proposed next version as the two script arguments.

## Claude reconciliation

Collection without `collect.WithClaudeReconciliation()` retains the existing
first-stored usage record for each dedup key. This can undercount a live Claude
message whose usage grows after its first pass. `store.ApplyBatch`,
`ApplyEvents` and `InsertEvents` keep those immutable insertion semantics.

The option uses the optional `ReconcileClaudeBatch` store method. It rereads
Claude sources each cycle, including files covered by old checkpoints, and
compares observations with actual stored usage. It changes only growing Claude
usage rows under these conditions:

- The total increases and no token counter decreases.
- Tool, model, provider, service tier, session, project, request and message
  identity match the stored record.
- A replacement cost prices the complete new observation. Unknown replacement
  cost clears the previous partial cost. Vendor cost is never replaced with an
  estimate.
- Usage ID, dedup key, timestamps, source path and raw payload remain unchanged.
  Existing calls and turn contexts therefore join the revised usage without
  duplicating logical turns or calls.

Usage, derived rollup, newly discovered attribution and checkpoint changes
commit in one transaction. Conflicting growth returns an error and rolls back
the batch. Older and equal-total observations are ignored. Returned insert
counts exclude revisions; callers using this mode should reread summaries
after successful cycles even when `EventsInserted` is zero. `IngestWatermark`
continues to describe new insertions, not revisions.

This option permits accounting updates that the default API forbids. Use one
collector per ledger and make the choice consistently for that ledger. It
does not reconcile decreasing/changed-identity revisions, replace missing
source history, or repair unrelated already-stamped historical pricing errors.
Full Claude rereads cost more than the default unchanged-file checkpoint gate.

## Durability and read consistency

Writable ledgers use WAL and `synchronous=FULL` on every pooled connection.
SQLite requests a WAL sync for each committed transaction. This strengthens
the previous `NORMAL` setting, which can lose acknowledged commits after an
OS crash or power failure. It also adds commit I/O. Durability depends on the
filesystem and hardware honoring sync requests; use a local filesystem that
supports SQLite locking, not a shared network filesystem. See
[SQLite's durability settings](https://www.sqlite.org/pragma.html#pragma_synchronous).

Grouped usage summaries calculate their buckets and distinct-session total
inside one read transaction. A concurrent collection commit cannot make those
parts describe different snapshots. Separate API calls can see different
committed snapshots, and `SQLITE_BUSY` remains possible under the documented
timeout. Stop collection before migration or restore, and keep backups outside
the live database bundle. The caller owns retention and recovery policy.

## Accounting guarantees

- Replaying the same event source deduplicates by stable usage identity.
  Source checkpoints and their observations are persisted together. Preserve
  the ledger's cumulative baselines and checkpoints between runs.
- Only one collector may write a given ledger at a time, across goroutines
  and processes. Concurrent collection of cumulative counters can count the
  same growth twice. The library does not acquire a process-wide collector lock.
- Use the source-normalized `TotalTokens`. Cache and reasoning buckets may
  overlap other reported counters; adding every field can double-count tokens.
- Costs are integer millionths of USD. A nil event cost is unknown, not zero.
  Summaries report unpriced events and distinguish computed costs from
  source-reported costs. Price provenance is an opaque label.
- Activity and turn-context rows do not create additional usage. Joining them
  must not multiply a turn's tokens or cost. Query one turn-context dimension
  at a time when aggregating its costs.
- Missing usage, unsupported source shapes, and unavailable counters stay
  missing. Copilot usage requires its OTEL file export. Crush reports accumulated
  cost only; its session token columns are not consumption totals.
- Synthetic cumulative usage is priced at base aggregate rates. Polling many
  short requests together does not make them a single long-context request.
  This remains an estimate because aggregate sources lack request boundaries.
- Historical price sync cannot reconstruct cache-write TTLs that were never
  stored. The built-in engine leaves a cost unknown when possible TTL splits
  change its value or provenance; equal-rate and verified free prices are safe.
  Custom pricing callbacks remain responsible for their own assumptions.
- Source records may grow, reset, arrive partially, or repeat across files.
  Adapter-specific accounting and recovery behavior is preserved by the
  fixture and regression tests. Changes to normalization, deduplication, or
  checkpoint meaning require tests that demonstrate both the old failure and
  the intended accounting result.

Collection can return partial results. Callers must check the returned error,
`CycleStats.Errors`, and `CycleStats.Canceled` instead of treating an incomplete
pass as complete. Preserve the distinction between a missing price and a free
request in downstream displays and exports.

Old Codex checkpoints are upgraded by rebuilding their parsing baseline from
the already-consumed prefix without emitting that prefix again. This prevents
new duplicate historical observations. Existing overcounts or incorrect
stamped prices produced by older code are not silently rewritten.

## Harness formats and security

[adapter/compatibility.json](adapter/compatibility.json) records the retained
source-version evidence; each adapter's `Capabilities()` describes the data it
can expose. Neither promises support for future harness formats or guarantees
that telemetry is enabled on a particular machine. Source-format updates must
include sanitized fixtures and focused regression checks.

Source roots are not a sandbox, and not every read has a memory bound. See the
[security policy](SECURITY.md) before embedding collection in a service that
accepts untrusted paths or files.
