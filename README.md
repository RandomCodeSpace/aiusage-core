# aiusage-core

Go library for collecting local coding-agent usage, storing an incremental
SQLite ledger, and calculating costs. Applications can import it directly;
there is no aiusage executable to install or subprocess to manage.

The library contains the shared collection packages extracted from
[aiusage](https://github.com/RandomCodeSpace/aiusage). It has no TUI, web
dashboard, application CLI, or service supervisor. See [SOURCE.md](SOURCE.md)
for the source commit, license, and extraction boundary.

Requires Go 1.25.13 or later. SQLite and zstd are pure Go, so builds work with
`CGO_ENABLED=0`.

```sh
go get github.com/RandomCodeSpace/aiusage-core
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
Your application owns cancellation, scheduling, logging, and process lifetime.
Run only one collector per ledger, including collectors in other processes.
Concurrent collection of cumulative counters can count the same growth twice.

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

The MIT license is in [LICENSE](LICENSE). The bundled LiteLLM snapshot retains
its source, license, and fetch metadata. The Models.dev snapshot retains its
[MIT attribution](pricing/data/modelsdev_LICENSE).
