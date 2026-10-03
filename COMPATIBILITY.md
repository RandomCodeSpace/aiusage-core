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

Patch releases preserve exported source compatibility and the accounting
meaning described below. During v0, an intentionally incompatible public API
change requires a minor release and a migration note. Changing the module
import path changes Go type identity: do not mix model or store types from the
original `aiusage` module with this module's types.

Consumers should pin a released module version when available. Until the first
release, pin an exact commit or Go pseudo-version. The store's SQL schema is
owned by the library, not a stable API for application writes. `store.Open`
applies forward migrations to the output ledger; opening a migrated ledger
with older library versions is not guaranteed. Back up the ledger before an
upgrade that changes its schema. Do not point the store at a harness database
or another application's database.

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
- Source records may grow, reset, arrive partially, or repeat across files.
  Adapter-specific accounting and recovery behavior is preserved by the
  fixture and regression tests. Changes to normalization, deduplication, or
  checkpoint meaning require tests that demonstrate both the old failure and
  the intended accounting result.

Collection can return partial results. Callers must check the returned error,
`CycleStats.Errors`, and `CycleStats.Canceled` instead of treating an incomplete
pass as complete. Preserve the distinction between a missing price and a free
request in downstream displays and exports.

## Harness formats and security

[adapter/compatibility.json](adapter/compatibility.json) records the retained
source-version evidence; each adapter's `Capabilities()` describes the data it
can expose. Neither promises support for future harness formats or guarantees
that telemetry is enabled on a particular machine. Source-format updates must
include sanitized fixtures and focused regression checks.

Source roots are not a sandbox, and not every read has a memory bound. See the
[security policy](SECURITY.md) before embedding collection in a service that
accepts untrusted paths or files.
