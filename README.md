# aiusage-core

[![Core CI](https://github.com/RandomCodeSpace/aiusage-core/actions/workflows/ci.yml/badge.svg?branch=main&event=push)](https://github.com/RandomCodeSpace/aiusage-core/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/RandomCodeSpace/aiusage-core)](https://github.com/RandomCodeSpace/aiusage-core/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/RandomCodeSpace/aiusage-core.svg)](https://pkg.go.dev/github.com/RandomCodeSpace/aiusage-core)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Go library for collecting local coding-agent usage, storing an incremental
SQLite ledger, and calculating costs. Applications can import it directly;
there is no aiusage executable to install or subprocess to manage.

The library contains the shared collection packages extracted from
[aiusage](https://github.com/RandomCodeSpace/aiusage). It has no TUI, web
dashboard, application CLI, or service supervisor. See [SOURCE.md](SOURCE.md)
for the source commit, license, and extraction boundary.

Requires Go 1.25.13 or later. SQLite and zstd are pure Go, so builds work with
`CGO_ENABLED=0`.

Linux and macOS are supported. See the
[compatibility contract](COMPATIBILITY.md) for tested Go versions, native tests,
cross-build coverage, and accounting guarantees, and the
[security policy](SECURITY.md) for reporting and trust boundaries.

```sh
go get github.com/RandomCodeSpace/aiusage-core@v0.1.2
```

## Embed a collector

Save this as `main.go` in your application module and run `go run .`. It reads
the current user's supported agent data and writes `usage.db` in the current
directory. Pricing uses the bundled snapshots without network requests.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/all"
	"github.com/RandomCodeSpace/aiusage-core/collect"
	"github.com/RandomCodeSpace/aiusage-core/pricing"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	ledger, err := store.Open("usage.db")
	if err != nil {
		log.Fatal(err)
	}
	defer ledger.Close()

	ctx := context.Background()
	prices := pricing.New(pricing.Options{})
	stats, err := collect.RunOnce(ctx, all.Default(), ledger,
		adapter.DiscoverConfig{Home: home},
		collect.WithPricer(prices), collect.WithoutRaw())
	if err != nil {
		log.Fatal(err)
	}
	for _, problem := range stats.Errors {
		log.Print(problem)
	}
	summary, err := ledger.Summarize(ctx, store.Filter{GroupBy: []string{"tool"}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("inserted=%d tokens=%d cost_micro_usd=%d unpriced=%d\n",
		stats.EventsInserted, summary.Totals.Total,
		summary.Totals.CostMicroUSD, summary.Totals.UnpricedEvents)
}
```

Use `adapter.NewRegistry(codex.New(), copilot.New())` with the corresponding
adapter imports to include only selected parsers. `adapter/all.Default()`
registers all 15 tools. Merely importing `adapter` does not import them.
`DiscoverConfig.Overrides` maps tool IDs to explicit source roots; each
adapter documents its layout and supported environment variables.

`collect.Run` performs an immediate pass, then repeats at the supplied interval
until its context is canceled. `collect.WithCycleCallback` reports every pass.
`collect.WithTrigger(requests)` also accepts on-demand requests through a
`<-chan struct{}`. Use a channel buffered to one and nonblocking sends to
coalesce requests without blocking a usage view:

```go
requests := make(chan struct{}, 1)
// Pass collect.WithTrigger(requests) to collect.Run.
select {
case requests <- struct{}{}:
default:
}
```

`collect.WithMinInterval(time.Minute)` limits both triggered and background
passes to at most one start per minute. Starts are spaced at least one second
by default; startup collection remains immediate. Requests made during a pass
schedule at most one later pass. Closing the channel disables triggers, and
`RunOnce` ignores both scheduling options.
These scheduling options are available starting with `v0.1.2`.

Your application owns cancellation, scheduling, logging, and process lifetime.
Run only one collector per ledger, including collectors in other processes.
Concurrent collection of cumulative counters can count the same growth twice.

Reuse the adapter registry across passes to keep the per-file parse cache from
`claudecode.New()` warm. Changed passes still deduplicate candidates across the
whole root. The cache is memory-only; after a restart, the next changed or full
pass reads all transcripts.
Claude workflow journals are excluded because they carry no usage. OpenCode
checkpoints record database and WAL file stamps, so unchanged databases are
skipped without opening SQLite.

For live Claude transcripts, `collect.WithClaudeReconciliation()` is an explicit
opt-in on versions containing that option. A streamed message can report more
tokens after its first observation. The default retains the first stored row;
the option rereads Claude sources and reconciles monotonic growth against the
ledger while preserving its identity and attribution. It requires a store with
`ReconcileClaudeBatch`, such as `*store.Ledger`; custom stores without that
capability return an error. Existing interfaces and defaults are unchanged.
See the [reconciliation contract](COMPATIBILITY.md#claude-reconciliation) before
enabling it. These additions are available starting with `v0.1.1`.

## Packages

| Package | Purpose |
| --- | --- |
| `model` | Usage, activity, capability, provenance, and checkpoint types |
| `adapter` | Discovery and collection contracts, registry, shared helpers |
| `adapter/<tool>` | Local source parsers, selectable by the caller |
| `adapter/all` | Registry containing every built-in adapter |
| `collect` | One pass or a cancellable collection loop |
| `store` | SQLite ledger, migrations, summaries, checkpoints, recovery |
| `pricing` | Embedded and cached price tables, overrides, cost calculation |

## Source prerequisites

Supported tools are Antigravity, Claude Code, Cline, Codex, Copilot, Crush,
DSH, Goose, Hermes, Kimi Code, OpenClaw, OpenCode, Pi, Qwen Code, and Reasonix.
The library reads files and databases those tools have already produced. It
does not query provider usage APIs, aggregate provider accounts, authenticate
to providers, or generate usage. Coverage depends on the local source format
and available records. Adapter `Capabilities()` and
[compatibility.json](adapter/compatibility.json) describe the retained evidence.

Copilot token usage requires its OpenTelemetry JSONL file export under
`~/.copilot/otel/`, or the file selected by
`COPILOT_OTEL_FILE_EXPORTER_PATH`. Its session database supplies recorded cost;
the database alone does not supply token usage. The application embedding this
library must arrange any required harness telemetry configuration.

Crush provides accumulated cost only. Its session token columns describe
context state, so this adapter does not report them as token consumption.
Zero-cost sessions produce no usage event. Missing source data stays missing.

Adapters open source SQLite databases read-only and do not update source rows.
SQLite may create WAL coordination sidecars when they are absent; a directory
that cannot support that coordination can fail to open. The output ledger and
optional price cache are separate files owned by the embedding application.

## Accounting and caches

Usage events deduplicate on stable keys. Incremental checkpoints are persisted
with the observations they account for. Preserve the ledger between passes:
it contains usage history, cumulative baselines, and checkpoints, not just a
disposable display cache. Choose a separate ledger per independent application
or coordinate ownership explicitly.

Token fields retain the source's accounting semantics. Cache reads and
reasoning may already belong to other reported buckets; consumers should use
`TotalTokens` and the pricing APIs instead of adding every field. Costs use
integer millionths of USD. A nil event cost and `UnpricedEvents` mean unknown,
not free. `ComputedCostEvents` identifies costs estimated from a price table.
Activity and turn-context rows are separate from usage; joining them must not
multiply the original turn's tokens or cost.

Automatic historical price sync uses `pricing.Engine.PriceStoredEvent` when
available. Stored events lack the cache-write lifetime split. If five-minute
and one-hour interpretations produce different costs or provenance, the row
stays unpriced. Equal-rate and verified free prices remain eligible. Supply a
pricer during collection to price fresh cache writes using their source data.
Custom pricers keep their existing `PriceEvent` callback unless they implement
the optional `PriceStoredEvent` method.

`pricing.New` reads embedded tables and an optional local cache without a
network request. To enable price metadata refresh, set both `DataDir` and
`Refresh: true`. Collection then offers the engine a refresh each cycle.
Successful LiteLLM refreshes are cached for 24 hours; failed refreshes retry on
the next cycle. Models.dev attempts at most once per 24 hours. Each request is
bounded to 10 seconds. Cached and embedded tables provide fallback. Model
overrides take precedence. No provider credentials are required.

`collect.WithoutRaw()` discards the allow-listed audit payload before storage.
Names, source paths, session IDs, and project paths can still be stored. The
model has no prompt or message-content field.

## Verification

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build ./...
```

The tests replay included fixtures and use temporary databases. The opt-in
long-ledger performance test requires `AIUSAGE_PERF_DB` and is skipped by
default. No production harness installation is needed.

`bash scripts/check-api.sh` checks the working tree against the released
`v0.1.0` API. It uses v1 compatibility rules and publishes nothing. See the
[production readiness review](PRODUCTION_READINESS.md) for findings, fixes,
verification limits and the remaining release gates.

## License

Released under the [MIT License](LICENSE). The bundled LiteLLM snapshot retains
its source, license, and fetch metadata. The Models.dev snapshot retains its
[MIT attribution](pricing/data/modelsdev_LICENSE).
