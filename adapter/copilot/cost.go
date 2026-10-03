package copilot

// Cost for GitHub Copilot, which is VENDOR-PRICED and never goes near the
// LiteLLM ladder.
//
// THE UNIT. Copilot meters every call in nano-AI-units. GitHub Docs (Usage-based
// billing for individuals) states "1 AI credit = $0.01 USD", and the vendor
// ships the arithmetic that produces the figure inline with each call:
// `assistant_usage_events.token_details_json` is an array of
// {tokenType, tokenCount, batchSize, costPerBatch}, and
// sum(tokenCount * costPerBatch / batchSize) reproduces `total_nano_aiu`
// EXACTLY on 45 of 45 local rows. The eight implied per-token rates across two
// models land on those models' published list prices to the cent, which is six
// independent confirmations of the one constant. So nanoAIU -> USD is a fixed
// division and Copilot needs no price table of ours.
//
// HONEST CAVEAT: this is list-price DENOMINATION, not necessarily what the
// account is invoiced. Plans carry credit allowances and an org may be on a
// different arrangement. It is the vendor's own valuation of the call.
// UNVERIFIED: whether nanoAiu ever reflects a negotiated rate.
//
// WHERE IT COMES FROM, AND WHY IT IS TWO PLACES. The number reaches this
// adapter by two routes, and the split is decided by which one is an IDENTITY
// rather than by which surface is nicer:
//
//   - A main-agent call carries `github.copilot.nano_aiu` on the very `chat`
//     span the usage row is built from. No join exists to get wrong. Verified
//     against the session store: on all 40 overlapping calls the span attribute
//     equals the store's `total_nano_aiu` to the nano-AIU, zero mismatches.
//   - A SUBAGENT call carries no such attribute — measured, 3 of 43 chat spans,
//     8.8% of the session's whole cost (1,663,250,000 of 18,913,170,000
//     nanoAIU), and it tracks the initiator rather than the outcome: a span that
//     failed with SessionDestroyedError still carries its cost, while a
//     subagent span that finished cleanly does not. That cost exists only in
//     `assistant_usage_events`, and it is joined by IDENTITY — see
//     attributeSubAgentCost.
//
// WHAT IS DELIBERATELY NOT JOINED. The remaining 40 store rows are NOT matched
// back to their spans, because they cannot be: the table has no provider-side
// call identifier — no service_request_id, no gen_ai.response.id, no apiCallId
// column. The only per-row handle both surfaces share is the token tuple
// (session, model, input, output, cache_read, cache_write), which is a CONTENT
// FINGERPRINT and not an identity: it happens to be unique on both sides here
// (45 of 45 and 43 of 43 distinct, 42 exact matches) and it would silently
// misprice the day two calls of a session billed the same counts. That is the
// codex rule — never a positional or content guess dressed as a join — and it
// costs nothing, because the number those rows would supply is already on the
// span, proven equal.
//
// THE CUMULATIVE TRAP. `session.usage_checkpoint.totalNanoAiu` and
// `session.shutdown.totalNanoAiu` are session-wide ACCUMULATED counters, not
// increments: summing the 12 local checkpoints yields 124,893,605,000 against a
// real 18,913,170,000, a 6.6x overstatement, and shutdown is a snapshot of the
// same counter rather than a further increment. Nothing in this package reads
// them — events.go has no field for them at all — and their one legitimate use
// is verification against the per-call sum, which lives in a test.
//
// CRITICAL: strictly read-only. mode=ro cannot create, write or lock;
// query_only(1) makes the connection refuse a write statement outright;
// immutable=1 is deliberately ABSENT because the CLI holds this database open
// and writes it live — on this machine the main file was 4,096 bytes against a
// 1.8 MiB WAL, so an immutable reader that ignores the WAL would have seen an
// empty table.

import (
	"database/sql"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0)
)

// driverName is the modernc.org/sqlite database/sql driver name.
const driverName = "sqlite"

// PriceSourceAIU labels a cost taken from Copilot's own nano-AIU valuation. The
// price_source vocabulary is open and nothing parses it; the copilot- prefix
// keeps these distinguishable from the ladder's rungs.
const PriceSourceAIU = "copilot-nano-aiu"

// nanoAIUPerMicroUSD converts the vendor's unit to the ledger's:
// 1e9 nanoAIU = 1 AIC = $0.01 = 10,000 micro-USD, so 1 micro-USD = 100,000
// nanoAIU.
const nanoAIUPerMicroUSD = 100_000

// microUSDFromNanoAIU converts a nano-AIU valuation to micro-USD, TRUNCATING.
// Rounding half up would let a window's summed cost exceed the vendor's own
// total for it by up to half a micro-USD per row; integer division can only
// ever understate, which is the direction this ledger is allowed to be wrong
// in — the same rule the activity divisor follows.
func microUSDFromNanoAIU(nano int64) int64 {
	if nano <= 0 {
		return 0
	}
	return nano / nanoAIUPerMicroUSD
}

// subAgentCostSQL sums the vendor's valuation per spawning tool call. Only rows
// naming a parent are selected: those are exactly the calls OTEL leaves
// unvalued, and reading the rest would be the content-fingerprint join this
// package refuses. No other column of the session store is read anywhere — the
// `turns` table holds whole prompts and assistant replies, and nothing here has
// a field for one.
const subAgentCostSQL = `
SELECT parent_tool_call_id, SUM(total_nano_aiu)
  FROM assistant_usage_events
 WHERE parent_tool_call_id IS NOT NULL
   AND TRIM(parent_tool_call_id) <> ''
   AND total_nano_aiu > 0
 GROUP BY parent_tool_call_id`

// attributeSubAgentCost fills in the vendor cost of subagent calls, whose spans
// carry none, and does so on an EXACT identity join.
//
// The chain is structural and lives entirely inside the span tree: a subagent's
// `chat` span's parent is an `invoke_agent task` span, whose parent is the
// `execute_tool task` span that spawned it, and THAT span's
// `gen_ai.tool.call.id` is the same string the store records as
// `parent_tool_call_id`. Measured: 3 of 3 tool-call ids matched, covering all 5
// subagent rows and the whole 8.8% OTEL omits.
//
// The shape is many store rows to ONE span — the export writes one `chat` span
// per spawned task however many API calls the subagent then made (locally one
// span covering 3 rows, two covering 1 each) — so this SUMS on the usage side.
// That is the mirror of the activity ledger's one-usage-row-to-many-calls case
// and wants no divisor.
//
// A tool call with MORE THAN ONE unvalued span beneath it is refused outright
// rather than divided or copied: copying would multiply the subagent's cost by
// the number of spans and dividing would invent a split the source does not
// record. Those rows stay unpriced, which is honest, and the cost is not lost
// from the store — only from our copy of it.
//
// A missing, unreadable or schema-changed store is not an error: the calls stay
// unpriced and the pass keeps its usage. It is also read at most once per OTEL
// file, and only when that file actually holds an unvalued call.
func attributeSubAgentCost(cands []*candidate, spans map[string]*otelRecord, dbPath string) {
	if dbPath == "" {
		return
	}
	// Resolve each unvalued candidate to the tool call that spawned it, and
	// count the candidates per tool call in the same pass.
	owners := make(map[*candidate]string)
	perCall := make(map[string]int)
	for _, c := range cands {
		if c.nanoAiu > 0 || c.source != srcChatSpan {
			continue
		}
		id := spawningToolCallID(c.rec, spans)
		if id == "" {
			continue
		}
		owners[c] = id
		perCall[id]++
	}
	if len(owners) == 0 {
		return
	}

	costs, err := loadSubAgentCosts(dbPath)
	if err != nil || len(costs) == 0 {
		return // unreadable or empty: unpriced, never zero
	}
	for c, id := range owners {
		if perCall[id] != 1 {
			continue // several spans under one spawn: no split the source records
		}
		c.nanoAiu = costs[id]
	}
}

// spawningToolCallID walks a subagent chat span up to the execute_tool span
// that spawned it and returns that span's provider call id. An empty result
// means the chain is not there, which is every main-agent call.
func spawningToolCallID(rec *otelRecord, spans map[string]*otelRecord) string {
	if rec == nil {
		return ""
	}
	agent := ancestorSpan(rec, spans, isAgentSummarySpan)
	if agent == nil || attrString(agent.Attributes, agentNameAttr) == "" {
		return "" // not a subagent turn: the session's own agent span names nothing
	}
	tool := ancestorSpan(agent, spans, isToolCallSpan)
	if tool == nil {
		return ""
	}
	return attrString(tool.Attributes, toolCallIDAttr)
}

// loadSubAgentCosts reads the per-spawn cost map from the session store.
func loadSubAgentCosts(path string) (map[string]int64, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	dsn := u.String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	rows, err := db.Query(subAgentCostSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var id sql.NullString
		var nano sql.NullInt64
		if err := rows.Scan(&id, &nano); err != nil {
			return nil, err
		}
		if !id.Valid || !nano.Valid || nano.Int64 <= 0 {
			continue
		}
		out[id.String] = nano.Int64
	}
	return out, rows.Err()
}
