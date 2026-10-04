# Production readiness review

Review date: 2026-10-04. Released baseline: `v0.1.0`, commit
`617d8d748269e96a1056ea9d9eaa372c2c752286`. The fixes are included in `v0.1.1`.
This document records the review evidence and the limits of production use.

## Decision and scope

The target is an embedded Go library on Linux and macOS, with trusted local
source files and one collector per ledger. The existing `v0.1.0` API must not
break. The user explicitly approved optional reconciliation for growing Claude
records while keeping current defaults.

The patch fixes the reproduced defects below, adds regression checks and an API
compatibility gate, and records the limits of the stability contract.
The `v0.1.1` release requires successful hosted CI on its exact commit.
Protected merging and acceptance in the consuming application remain
requirements for production sign-off, not claims made by publishing this patch.

Out of scope: concurrent collectors, hostile uploads or source trees, Windows,
new harness integrations, dependency upgrades, revaluation of existing
incorrect history, and a service availability or response-time SLA.

## Findings and changes

| Finding | Evidence at baseline | Change | Remaining limit |
| --- | --- | --- | --- |
| P1: Codex cumulative double count | A last+total record of 110 followed by total-only 170 emitted 280 total tokens | Always retain the cumulative baseline; upgrade legacy checkpoint state by reconstructing the prefix without re-emitting it | Previously stored overcounts are not rewritten |
| P1: Claude streaming undercount | A message stored at 15, later observed at 60, remained at 15 | Opt-in ledger-backed reconciliation of monotonic growth, with atomic rollup and checkpoint updates | Default stays immutable; non-monotonic and changed-identity revisions are not reconciled |
| P1: aggregate long-context overcharge | A 300,000-token session aggregate was charged 600,000 microUSD at a 200,000-token request threshold instead of the 300,000 base estimate | Preserve aggregate meaning through its existing synthetic identity during immediate and historical pricing | Aggregate sources do not contain enough data to reconstruct exact per-request tier charges |
| P1: historical cache-write undercharge | One million one-hour cache-write tokens priced later became 1,250,000 microUSD instead of 2,000,000 | Optional `Engine.PriceStoredEvent` rejects TTL-dependent historical prices; fresh pricing retains the split | The discarded split cannot be recovered; equal-rate and verified free prices remain available |
| P1: acknowledged commits lack power-loss durability | Runtime pragma returned `synchronous=1`, NORMAL | Writable connections use FULL and regression tests verify replacement pooled connections | Actual power-loss behavior depends on filesystem and hardware; no physical power-cut test was run |
| P2: mixed summary snapshots | A writer commit between grouping and distinct-session queries produced one event and two sessions | Both queries use one read transaction in ledger and rollup paths | Separate API calls may observe different committed snapshots |
| P2: linked Claude transcript growth skipped | Appending to a regular-file symlink target produced no second-pass events | Manifest checkpoints use target metadata, matching the parsed file | Source roots remain trusted and are not a filesystem sandbox |
| P2: final-source cancellation reported success | Final source canceled context but returned nil error and `Canceled=false` | Check cancellation at cycle entry and final return | Cancellation still cannot interrupt every OS filesystem operation |
| Release gate: compatibility only documented | No public API diff gate existed | Pinned Go `gorelease` check against `v0.1.0`, under v1 rules, added to Linux integration CI | GitHub must require that job before merging |

All runtime findings except the physical power-loss consequence were reproduced
using synthetic data. The NORMAL/FULL distinction follows
[SQLite's documented durability semantics](https://www.sqlite.org/pragma.html#pragma_synchronous).
The new abrupt-process-exit test verifies committed usage survives while
uncommitted usage and checkpoint changes roll back. It is not a power-cut test.

## API compatibility

No existing exported function, method, field, constant or required interface
member is removed or changed. No module dependency or database schema version
changes. The additions are:

- `collect.WithClaudeReconciliation()`
- `(*store.Ledger).ReconcileClaudeBatch(ctx, batch)`
- `(*pricing.Engine).PriceStoredEvent(event)`

Both new capabilities are optional. Existing custom stores still satisfy
`collect.Store`; existing pricers continue to use `PriceEvent`. An explicit
reconciliation request on an unsupported store returns an error rather than
silently retaining immutable behavior.

The gate uses
[`golang.org/x/exp/cmd/gorelease`](https://pkg.go.dev/golang.org/x/exp/cmd/gorelease)
at `v0.0.0-20260908205506-85c1c2202aba`. This is a Go-team experimental
development tool under BSD-3-Clause, not a runtime library dependency. Its
module has a September 2026 revision; an exact-version OSV query returned no
advisories during this review. The version is pinned in the script. The wrapper
also rejects the tool's incompatible-change report: gorelease alone permits
incompatibilities when its baseline is v0, even for a proposed v1 version.
The negative test exposed that behavior before the gate was accepted.
The check publishes no version.

The compatibility check passes for the current patch. A negative test removed
`collect.WithoutRaw` in an isolated temporary copy and verified that the tool
rejects the incompatible API. Source comparison cannot establish behavior,
which is why the regression tests remain separate acceptance checks.

## Reconciliation contract

Opt-in reconciliation updates the full token/cost snapshot only when the total
grows, every token bucket is nondecreasing, and billing/usage identities match.
It does not combine bucket maxima from different observations. A missing new
cost becomes unknown; the previous partial cost is not presented as complete.
Existing vendor cost cannot be replaced by a computed estimate.

The original usage identity remains intact, so activity and context queries
retain one logical usage row and derive the revised amounts through their
existing joins. New attribution, counters, cost, rollup and source checkpoint
share one transaction. The update guard is validated, temporarily suspended
and restored inside that transaction. A checkpoint failure rolls everything
back, including the guard.

The default insertion API is still immutable. Insert counts and
`IngestWatermark` retain their old meaning and do not count/signal revisions.
An application opting into reconciliation should reread its summaries after a
successful cycle, including cycles with no new events. Original timestamps and
raw payloads remain unchanged. The complete contract is in
[COMPATIBILITY.md](COMPATIBILITY.md#claude-reconciliation).

## Verification evidence

Tests use synthetic fixtures and temporary ledgers. They do not read the user's
live harness data. The following checks completed locally on Linux:

| Check | Scope | Result |
| --- | --- | --- |
| Minimum toolchain | Go 1.25.13, CGO disabled; complete tests in `collect`, `pricing`, `store`, `adapter/codex`, `adapter/claudecode` | Passed |
| Current toolchain | Go 1.26.6, CGO disabled; affected package suites and focused reruns after relevant changes | Passed |
| Race detector | Go 1.26.6; focused reconciliation, snapshot, durability, cancellation, pricing, adapter and crash-recovery regressions in those five packages | Passed |
| Public API | Final working tree against published `v0.1.0` | Passed; three compatible additions |
| API gate rejection | Isolated temporary copy with `collect.WithoutRaw` removed | Correctly exited 1 |
| Vulnerability scan | `govulncheck` on those five packages with Go 1.26.6 | No vulnerabilities found |
| Workflow and patch syntax | `actionlint`, `bash -n`, `git diff --check` | Passed |

No repository-wide local test suite was run. Native macOS and hosted CI results
are recorded separately in the release notes at publication.

- Before fixing the owning logic, focused regressions failed for cancellation,
  cache pricing, aggregate pricing, mixed Codex records, linked transcripts,
  inconsistent summaries and the durability pragma. The isolated Claude test
  reproduced 15 stored tokens versus 60 observed tokens.
- New reconciliation checks cover defaults, legacy checkpoints, idempotent
  replay, attribution, unknown replacement costs, conflicting identities,
  non-monotonic counters, stale rollup, modified update guard and rollback.
- Existing tests cover checkpoint failure, migration/restore, price-sync
  protection, deduplication, source parsing and query contracts in the affected
  packages. New tests also exercise abrupt process termination and recovery.
- API compatibility is checked against the published module through the Go
  proxy, with an isolated incompatible-change experiment to prove rejection.
- Workflow syntax is checked with `actionlint`; shell syntax and whitespace
  are checked for the edited files. Release publication also requires hosted CI.

Use these scoped checks when the corresponding code changes:

```sh
GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go test ./collect ./pricing ./store ./adapter/codex ./adapter/claudecode -count=1
GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 bash scripts/check-api.sh
GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 govulncheck ./collect ./pricing ./store ./adapter/codex ./adapter/claudecode
actionlint .github/workflows/ci.yml
```

Do not interpret a passing fixture suite as certification of every harness
version. The independent adapter review inspected shared helpers and selected
Codex, Claude, Cline, Pi and Goose paths; it was not a line-by-line audit of all
15 adapters. The compatibility manifest's ready labels describe retained
evidence for particular versions and environments.

## Release and production acceptance

1. Before publication, review and commit the patch, then obtain successful hosted CI on that exact
   commit. The existing native matrix covers Linux amd64 on minimum/current Go
   and macOS arm64 on current Go. Linux arm64 and macOS amd64 remain cross-build
   coverage, not native runtime certification.
2. For ongoing production maintenance, require the existing CI jobs in GitHub before merging: `Minimum Go /
   linux-amd64`, `Current Go / linux-amd64`, `Current Go / darwin-arm64`,
   `Race detector / linux-amd64`, and `Vulnerability scan`. The API check is a
   mandatory step in `Current Go / linux-amd64`. On review, the ruleset list was
   empty and the branch-protection endpoint returned `Branch not protected`.
   Repository policy was not changed by this patch.
3. Before production sign-off, exercise the consuming application's real collection cadence, ledger size,
   backup and restore process. FULL adds commit sync I/O and Claude opt-in
   rereads its sources. No workload latency target or production dataset was
   supplied, so workload performance and a recovery-time objective are unverified.
4. Publish `v0.1.1` after the exact-commit CI gate, with the remaining production
   limits stated in its release notes. Keep `v0.1.0` unchanged.
   Verify release metadata, tag commit, public Go-proxy download and checksums.
   A v1 tag is a compatibility commitment, not a promise of zero defects.

## Report-only follow-up

- Cline's array decoder can suppress some type errors after partial decoding;
  inspect whether malformed counters can be accepted as zero before treating
  this as a confirmed bug. This review did not reproduce it.
- Some Goose read paths suppress row/activity errors while withholding the
  checkpoint. User-visible diagnostic completeness warrants a focused follow-up;
  no additional defect was established here.
- Existing incorrect stamped costs and Codex overcounts remain historical
  data. Correcting them requires an explicit recovery/reconciliation policy;
  this patch does not silently rewrite them.
- Physical power-loss testing, hostile-input resource isolation and native
  runtime tests on Linux arm64/macOS amd64 remain outside the verified scope.
