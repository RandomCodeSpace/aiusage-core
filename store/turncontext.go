// Turn-context cost attribution: what did a skill, an agent, an MCP server or a
// plugin actually cost.
//
// usage_events answers "what did this turn cost"; activity_events answers "what
// did the agent call". Neither answers "what did X cost", and the reason is
// worth stating because it looks like it should.
//
// An activity row of kind=skill records the turn that INVOKED a skill — one
// call, one row. The work the skill then goes on to do is thousands of further
// turns, and none of them is a Skill call. Locally that gap is 44 invocation
// rows against 8,039 records of actual work: asking activity_events which skill
// is expensive returns the cost of pressing the button, not the cost of the
// thing the button started. For subagents the gap is not a gap but a chasm —
// 79,816 usage-bearing records ran as a subagent, 77.6% of all token-bearing
// turns on this machine, described by a couple of hundred `Agent` call rows.
//
// The missing facts live on the source record. Claude Code stamps every
// assistant record with up to FIVE top-level attribution strings —
// attributionAgent, attributionSkill, attributionMcpTool, attributionMcpServer,
// attributionPlugin — naming what the turn was running under along each axis.
// Those are properties of the TURN, not extra calls within it, and the whole
// design follows from that one observation.
//
// # ONE TABLE, FIVE DIMENSIONS, AND WHY THAT IS NOT FOUR TABLES
//
// Each of the five could have had its own table on the model usage_skill_context
// set. It should not, and the giveaway is that the second one would have been a
// verbatim copy of the first with a column renamed: same key, same 1:1 join to
// the ledger, same absent divisor, same append-only triggers, same three
// indexes. A table per dimension makes the SIXTH attribution axis a schema
// migration, five near-identical query builders that can drift apart, and five
// places to remember the partition rule. usage_turn_context carries the axis as
// a COLUMN, so a new dimension is a new CHECK value and nothing else.
//
// # WHY THIS CANNOT DOUBLE-COUNT WITHIN A DIMENSION
//
// A usage row carries at most one value per dimension — every one of the five
// source fields is a scalar string, verified across 99,894 occurrences in the
// local corpus, 100% of them JSON strings — and this table makes that a database
// constraint rather than a hope: (usage_dedup_key, dimension) is the PRIMARY
// KEY. usage_events.dedup_key is likewise UNIQUE. Once a query is pinned to ONE
// dimension the join below is therefore 1:1 in both directions and cannot
// multiply a row, so SUM(u.cost_micro_usd) over it adds each turn's cost AT MOST
// ONCE. There is no divisor here and nothing to share, because nothing competes:
// unlike the tool-call split, which divides one turn's cost among the calls that
// shared it, a turn context IS the turn.
//
// # WHY IT CAN DOUBLE-COUNT ACROSS DIMENSIONS, AND WHAT STOPS IT
//
// That "pinned to one dimension" is load-bearing and is the price of the single
// table. A turn commonly carries three or four contexts at once — measured:
// 3,816 records carry agent+mcp_tool+mcp_server, 2,201 carry agent+skill+plugin,
// 9 carry all five — and EVERY one of those rows names the turn's full cost,
// because each is a complete answer to a different question. A query that
// forgets the dimension predicate joins such a turn once per context and reports
// up to five times the real spend.
//
// So the dimension is not a filter field that a caller might leave unset. It is
// a REQUIRED ARGUMENT of every read below, validated against the closed
// vocabulary before any SQL is built, and stitched into the WHERE clause by the
// builder rather than by the caller. Grouping by "dimension" — the one grouping
// that would put two partitions in one result set — is refused by name, as is
// grouping by any dimension OTHER than the one being queried. There is no code
// path in this package that reads two dimensions in one statement.
//
// These five plus the tool-call attribution in activity_events are SIX
// PARTITIONS OF THE SAME DOLLARS, in the way cost-by-region and cost-by-product
// are two views of one budget: each honest alone, meaningless added together. No
// query in this package reads usage_turn_context and activity_events at once,
// for the same reason.
//
// # WHY NOT A COLUMN ON activity_events
//
// Because most of the fact would vanish. A turn can run under a skill or as a
// subagent and call no tool at all — thinking, planning, reading its own output
// — and it emits no activity row to hang a column on. Measured: 3,361 of 8,039
// skill-context records carry zero tool_use blocks (41.8%), and the agent
// dimension covers call-less turns far more often still. Keying on the usage row
// instead of on a call captures those for free, because the usage row is what
// exists in every case.
//
// # WHAT THIS DELIBERATELY DOES NOT DO
//
// It does not compose with the activity ledger's Kinds/Names filters, and the
// queries refuse them rather than ignoring them. "Skill cost among turns that
// called Bash" sounds reasonable and is a trap: reaching it means joining
// activity_events, at which point a turn with two Bash calls joins twice and its
// cost doubles. The refusal is the guard rail, not tidiness.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// turnContextTableDDL is usage_turn_context's CREATE TABLE as the v7 migration
// applies it. schema.sql carries the same table (it always describes the full
// latest schema) and TestTurnContextTableMatchesFreshSchema compares the two, so
// a migrated database and a fresh one can never carry different tables under one
// version stamp.
//
// (usage_dedup_key, dimension) is the PRIMARY KEY and that is the load-bearing
// line of the whole feature: it is what makes "at most one value per turn per
// axis" a constraint the database enforces instead of an invariant a reader has
// to trust, while still letting one turn carry a value on every axis at once.
// It is deliberately NOT a foreign key, for the same reason activity_events'
// join column is not: a context is an observed fact even when its usage row was
// skipped as a poison row, and the read path left-joins so a missing partner
// contributes no cost instead of losing the observation.
//
// The dimension CHECK closes the vocabulary at the storage layer. An adapter
// cannot invent a sixth axis by writing a new string: a new dimension is a
// migration, which is what keeps the read API's per-dimension refusals honest.
const turnContextTableDDL = `CREATE TABLE IF NOT EXISTS usage_turn_context (
  usage_dedup_key    TEXT    NOT NULL,
  dimension          TEXT    NOT NULL,
  value              TEXT    NOT NULL,
  tool               TEXT    NOT NULL,
  session_id         TEXT    NOT NULL DEFAULT '',
  project            TEXT    NOT NULL DEFAULT '',
  model              TEXT    NOT NULL DEFAULT '',
  event_time_unix    INTEGER NOT NULL,
  observed_time_unix INTEGER NOT NULL,
  source_path        TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (usage_dedup_key, dimension),
  CHECK (usage_dedup_key <> ''),
  CHECK (dimension IN ('agent','skill','mcp_tool','mcp_server','plugin')),
  CHECK (value <> '')
)`

// turnContextIndexDDL and turnContextTriggerDDL complete the v7 step. Every
// index leads with `dimension`, because every query is pinned to one and a
// dimension-less index would be scanned across partitions it can never serve.
// The triggers make this table append-only on the same terms as the two
// ledgers: a turn context is an observation of an immutable transcript line, not
// working state, so there is no legitimate reason to rewrite one.
var (
	turnContextIndexDDL = []string{
		`CREATE INDEX IF NOT EXISTS idx_turnctx_dim_value_time ON usage_turn_context(dimension, value, event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_turnctx_dim_time       ON usage_turn_context(dimension, event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_turnctx_session        ON usage_turn_context(session_id)`,
	}
	turnContextTriggerDDL = []string{
		`CREATE TRIGGER IF NOT EXISTS trg_turnctx_no_update
BEFORE UPDATE ON usage_turn_context
BEGIN SELECT RAISE(ABORT, 'usage_turn_context is append-only: UPDATE forbidden'); END`,
		`CREATE TRIGGER IF NOT EXISTS trg_turnctx_no_delete
BEFORE DELETE ON usage_turn_context
BEGIN SELECT RAISE(ABORT, 'usage_turn_context is append-only: DELETE forbidden'); END`,
	}
)

// skillContextV6Statements is FROZEN HISTORY: usage_skill_context's v6 step,
// preserved verbatim even though v7 immediately drops the table it creates.
//
// A migration step is not a description of the current schema, it is the
// definition of what "v6" means, and a database that stops at v6 — because the
// process died between the two commits, or because an older binary wrote it —
// must be exactly what this produced. Rewriting v6 into a no-op now that v7
// supersedes it would leave applyMigration's version arithmetic vouching for a
// database state that never existed. A v5 database upgrading today therefore
// creates this table and drops it one transaction later, which costs a
// millisecond and keeps every version stamp meaning one thing.
func skillContextV6Statements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS usage_skill_context (
  usage_dedup_key    TEXT    NOT NULL PRIMARY KEY,
  tool               TEXT    NOT NULL,
  skill              TEXT    NOT NULL,
  session_id         TEXT    NOT NULL DEFAULT '',
  project            TEXT    NOT NULL DEFAULT '',
  model              TEXT    NOT NULL DEFAULT '',
  event_time_unix    INTEGER NOT NULL,
  observed_time_unix INTEGER NOT NULL,
  source_path        TEXT    NOT NULL DEFAULT '',
  CHECK (usage_dedup_key <> ''),
  CHECK (skill <> '')
)`,
		`CREATE INDEX IF NOT EXISTS idx_skillctx_skill_time ON usage_skill_context(skill, event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_skillctx_event_time ON usage_skill_context(event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_skillctx_session    ON usage_skill_context(session_id)`,
		`CREATE TRIGGER IF NOT EXISTS trg_skillctx_no_update
BEFORE UPDATE ON usage_skill_context
BEGIN SELECT RAISE(ABORT, 'usage_skill_context is append-only: UPDATE forbidden'); END`,
		`CREATE TRIGGER IF NOT EXISTS trg_skillctx_no_delete
BEFORE DELETE ON usage_skill_context
BEGIN SELECT RAISE(ABORT, 'usage_skill_context is append-only: DELETE forbidden'); END`,
	}
}

// dropSkillContextDDL retires usage_skill_context, which usage_turn_context
// subsumes exactly: its rows are this table's dimension='skill' partition, and
// keeping both would leave two tables answering one question with no mechanism
// keeping them in step.
//
// A DROP in a migration is a serious thing and needs its justification stated.
// usage_skill_context is DERIVED, not history: every row of it is re-derived
// from the source transcript on the next collection pass, exactly as
// usage_rollup is re-derived from the ledger. Nothing in it originates there —
// unlike usage_events, whose rows are the only copy of a fact the provider will
// never repeat. Dropping it therefore loses no information, only a cache, which
// is the same test usage_rollup passes and the reason RebuildRollup is always a
// safe repair.
//
// The triggers are dropped FIRST and explicitly. DROP TABLE is documented not to
// fire row triggers, but the no-DELETE trigger exists precisely to make that
// class of statement fail, and relying on an exemption to get past a guard is a
// worse bet than removing the guard on purpose in the same transaction.
var dropSkillContextDDL = []string{
	`DROP TRIGGER IF EXISTS trg_skillctx_no_update`,
	`DROP TRIGGER IF EXISTS trg_skillctx_no_delete`,
	`DROP TABLE IF EXISTS usage_skill_context`,
}

// resetClaudeCheckpointDDL is what makes the drop above cost nothing, and it is
// the second half of the same argument.
//
// "It is re-derived on the next pass" is only true if a next pass READS the
// source again. The claude-code adapter gates a whole root on a size+mtime
// manifest and returns without opening a file when nothing changed, so on a
// machine whose transcripts happen to be idle the dropped skill contexts would
// stay dropped and the four NEW dimensions would never arrive at all — an
// upgrade that empties a table and refills it "eventually, if you use the tool".
// Clearing the checkpoint forces exactly one full re-parse, after which the gate
// resumes as before.
//
// DELETE on source_checkpoints is permitted where it would not be on the
// ledgers: checkpoints are mutable working state (no append-only triggers, one
// row per source, rewritten every cycle), and their loss costs a re-read, never
// a fact. The re-read is idempotent by construction — usage and activity rows
// re-derive their existing dedup keys and conflict-skip, so the pass appends
// only the turn contexts that are genuinely new.
//
// It is scoped to claude-code because claude-code is the only adapter that
// emits turn contexts; re-reading the others would be work with no possible
// result.
var resetClaudeCheckpointDDL = []string{
	`DELETE FROM source_checkpoints WHERE tool = '` + model.ToolClaudeCode + `'`,
}

// turnContextV7Statements is the ordered v7 migration body: create the general
// table with its indexes and triggers, retire the table it subsumes, then clear
// the checkpoint that would otherwise stop the data coming back.
func turnContextV7Statements() []string {
	out := make([]string, 0,
		1+len(turnContextIndexDDL)+len(turnContextTriggerDDL)+
			len(dropSkillContextDDL)+len(resetClaudeCheckpointDDL))
	out = append(out, turnContextTableDDL)
	out = append(out, turnContextIndexDDL...)
	out = append(out, turnContextTriggerDDL...)
	out = append(out, dropSkillContextDDL...)
	out = append(out, resetClaudeCheckpointDDL...)
	return out
}

// TurnContextBucket is one grouped row of turn-context cost along ONE dimension.
//
// The token and cost fields carry NO "Attributed" prefix, and the omission is
// deliberate: unlike ActivityBucket's divided shares, these are the joined usage
// rows' FULL counts. Within the queried dimension each turn belongs to exactly
// one bucket, so no share is taken and none is lost. A bucket's cost is the real
// ledger cost of the turns that ran under that value, not an estimate of it.
type TurnContextBucket struct {
	// Keys maps each GroupBy dimension to its value for this bucket
	// (e.g. {"value":"adhd"}). Ordered via OrderedKeys.
	Keys        map[string]string
	OrderedKeys []string

	// Turns counts usage rows that ran under this context — one per provider
	// record, not per tool call.
	Turns int64
	// Sessions is the distinct non-empty session count within the group.
	// Distinct counts do not add across buckets.
	Sessions int64

	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	// CostMicroUSD sums the costs STAMPED on the joined usage rows. Turns whose
	// usage row carries no stamped cost contribute nothing and are counted in
	// UnpricedTurns instead, so a bucket with UnpricedTurns > 0 is an
	// understatement until those are display-priced.
	CostMicroUSD int64

	// UnjoinedTurns counts context rows whose usage row is absent from the
	// ledger — a poison row the insert skipped, or a ledger pruned under the
	// context. It should be zero in practice, since a context is only ever
	// emitted alongside an accepted usage event, and it is reported rather than
	// assumed because a silent zero would be indistinguishable from free.
	UnjoinedTurns int64
	// UnpricedTurns counts turns that DID join a usage row carrying no stamped
	// cost (collected before v3, or a model the price ladder could not price).
	// Their tokens are counted; their cost is not.
	UnpricedTurns int64
	// ComputedCostTurns counts turns whose joined usage row was priced from a
	// public rate card rather than by the harness (model.PriceProvenance). Zero
	// means every dollar in CostMicroUSD traces to a vendor's own number.
	ComputedCostTurns int64
}

// TurnContextSummary is the result of SummarizeTurnContext: grouped buckets plus
// a grand total, both scoped to the single dimension the query named.
type TurnContextSummary struct {
	// Dimension is the axis these buckets partition. It is echoed back because
	// a summary is meaningless without it: two summaries on different
	// dimensions cover the same dollars and must never be concatenated.
	Dimension model.TurnDimension
	GroupBy   []string
	Buckets   []TurnContextBucket
	Totals    TurnContextBucket
}

// SkillCostBucket and SkillCostSummary are the skill-flavoured names for the
// general types. They are ALIASES, not copies: there is exactly one struct, one
// query builder and one table behind them, so the two spellings cannot drift
// into two answers to the same question.
type (
	SkillCostBucket  = TurnContextBucket
	SkillCostSummary = TurnContextSummary
)

// turnContextSelectSQL is the metric half of both queries' select list, in the
// order queryTurnContextBuckets scans it. Note the absence of any division: see
// the package comment on why a turn context has no divisor.
//
// It is a var rather than a const only because its last column is assembled
// from model's vendor price-source vocabulary at init (computedCostCountSQL);
// nothing writes to it after that.
var turnContextSelectSQL = `COUNT(*),
	COUNT(DISTINCT CASE WHEN c.session_id <> '' THEN c.session_id END),
	COALESCE(SUM(u.input_tokens),0),
	COALESCE(SUM(u.output_tokens),0),
	COALESCE(SUM(u.total_tokens),0),
	COALESCE(SUM(u.cost_micro_usd),0),
	COALESCE(SUM(CASE WHEN u.dedup_key IS NULL THEN 1 ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN u.dedup_key IS NOT NULL AND u.cost_micro_usd IS NULL THEN 1 ELSE 0 END),0),
	` + computedCostCountSQL("u.cost_micro_usd", "u.price_source")

// turnContextFromSQL is the joined source. Both sides of the ON are unique keys
// — usage_turn_context's PRIMARY KEY, once the dimension is pinned, and
// usage_events' UNIQUE dedup_key — so this join is 1:1 and structurally
// incapable of counting a turn twice. The pin is not optional: see
// buildTurnContextWhere.
const turnContextFromSQL = ` FROM usage_turn_context c
	LEFT JOIN usage_events u ON u.dedup_key = c.usage_dedup_key`

// turnContextOrderExpr maps a rank metric to its SQL ordering expression,
// reusing ActivityOrder's vocabulary. ActivityByCalls ranks by TURNS here: a
// turn context counts turns, and there is no per-call row to count.
func turnContextOrderExpr(o ActivityOrder) (string, error) {
	switch o {
	case ActivityByCalls, "":
		return "COUNT(*)", nil
	case ActivityByCost:
		return "COALESCE(SUM(u.cost_micro_usd),0)", nil
	case ActivityByTokens:
		return "COALESCE(SUM(u.total_tokens),0)", nil
	default:
		return "", fmt.Errorf("store: invalid activity order %q", o)
	}
}

// turnGroupExpr maps a GroupBy dimension to its SQL select/group expression,
// for a query already pinned to the turn dimension dim. The time dimensions use
// the SAME layouts groupExpr and activityGroupExpr use, so a context bucket key
// and a usage bucket key mean the same thing and the two can be put side by
// side. As there, 'localtime' follows the SYSTEM timezone rather than Go's
// time.Local.
//
// THE TWO REFUSALS ARE THE PARTITION INVARIANT, ENFORCED.
//
// "dimension" is refused because grouping by it is EXACTLY the double count:
// it would put agent rows and skill rows in one result set, each naming the same
// turns' full cost, and a reader summing the column would get several times the
// window's real spend from a query that never looked wrong.
//
// Another dimension's NAME is refused for the same reason one step removed.
// The queried dimension's own name is accepted as an alias for "value" because
// `GroupBy: []string{"skill"}` on a skill query reads better than "value" and
// means the same thing — but `GroupBy: []string{"agent"}` on a skill query does
// NOT mean anything: there is no agent column to group by, only the skill
// partition's values, and silently grouping them under the heading "agent" would
// mislabel every row. Refusing it is what keeps the alias from becoming a way to
// pretend one partition is another.
//
// "kind" and "name" are absent for the older reason: they are activity_events
// dimensions, and this table holds no calls to group by.
func turnGroupExpr(dim model.TurnDimension, g string) (string, error) {
	switch g {
	case "hour":
		return "strftime('%Y-%m-%d %H', c.event_time_unix, 'unixepoch', 'localtime')", nil
	case "day":
		return "strftime('%Y-%m-%d', c.event_time_unix, 'unixepoch', 'localtime')", nil
	case "week":
		return "strftime('%Y-W%W', c.event_time_unix, 'unixepoch', 'localtime')", nil
	case "month":
		return "strftime('%Y-%m', c.event_time_unix, 'unixepoch', 'localtime')", nil
	case "value", string(dim):
		return "c.value", nil
	case "tool":
		return "c.tool", nil
	case "project":
		return "c.project", nil
	case "session":
		return "c.session_id", nil
	case "model":
		return "c.model", nil
	case "dimension":
		return "", fmt.Errorf(
			"store: grouping by %q would put several turn-context dimensions in one result, "+
				"and they are partitions of the same tokens rather than parts of a total; "+
				"run one query per dimension instead", g)
	case "kind", "name":
		return "", fmt.Errorf(
			"store: %q groups activity calls, not turn contexts; group by %q instead", g, "value")
	default:
		if model.TurnDimension(g).Valid() {
			return "", fmt.Errorf(
				"store: this query is pinned to the %q dimension and cannot group by %q; "+
					"the two attribute the same tokens along different axes, "+
					"so ask for %q in a separate SummarizeTurnContext call", dim, g, g)
		}
		return "", fmt.Errorf("store: invalid turn-context group dimension %q", g)
	}
}

// turnGroupExprs resolves every dimension of a filter, refusing the whole query
// on the first unusable one.
func turnGroupExprs(dim model.TurnDimension, f ActivityFilter) ([]string, error) {
	out := make([]string, 0, len(f.GroupBy))
	for _, g := range f.GroupBy {
		expr, err := turnGroupExpr(dim, g)
		if err != nil {
			return nil, err
		}
		out = append(out, expr)
	}
	return out, nil
}

// checkTurnContextFilter validates the dimension and refuses the parts of an
// ActivityFilter this table cannot honour, rather than ignoring them. Silently
// dropping a filter the caller set would answer a different question than the
// one asked while looking like it had obeyed, and for Kinds/Names honouring them
// would require a join to activity_events that re-introduces the multi-call
// double count the whole design exists to prevent.
func checkTurnContextFilter(dim model.TurnDimension, f ActivityFilter) error {
	if !dim.Valid() {
		return fmt.Errorf("store: unknown turn-context dimension %q; want one of %v",
			dim, model.TurnDimensions())
	}
	if len(f.Kinds) > 0 {
		return fmt.Errorf("store: ActivityFilter.Kinds selects activity kinds and cannot restrict turn contexts; drop it")
	}
	if len(f.Names) > 0 {
		return fmt.Errorf("store: ActivityFilter.Names selects tool/skill/hook call names and cannot restrict turn contexts; use Values instead")
	}
	// Skills is the pre-generalisation spelling of Values and only means
	// anything on the skill partition. Applying it to another dimension would
	// filter agent names by a list of skill names and return nothing, which
	// reads as "that agent cost nothing" rather than as the mistake it is.
	if len(f.Skills) > 0 && dim != model.DimensionSkill {
		return fmt.Errorf(
			"store: ActivityFilter.Skills restricts the %q dimension and cannot restrict %q; use Values instead",
			model.DimensionSkill, dim)
	}
	return nil
}

// buildTurnContextWhere builds the WHERE clause and positional args for one
// dimension's slice of the table.
//
// The dimension predicate is unconditional and is added HERE rather than by any
// caller. That is the difference between an invariant and a convention: there is
// no argument a caller can pass, and no combination of empty filters, that
// produces a statement without it, so no read in this package can span two
// partitions.
func buildTurnContextWhere(dim model.TurnDimension, f ActivityFilter) (string, []any) {
	conds := []string{"c.dimension = ?"}
	args := []any{string(dim)}

	if !f.Since.IsZero() {
		conds = append(conds, "c.event_time_unix >= ?")
		args = append(args, f.Since.UTC().Unix())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "c.event_time_unix < ?")
		args = append(args, f.Until.UTC().Unix())
	}

	addIn := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		conds = append(conds, col+" IN ("+strings.Join(ph, ",")+")")
	}
	addIn("c.tool", f.Tools)
	addIn("c.value", f.Values)
	// Skills is only reachable here on the skill dimension; checkTurnContextFilter
	// refuses it everywhere else.
	addIn("c.value", f.Skills)
	addIn("c.project", f.Projects)
	addIn("c.session_id", f.Sessions)
	addIn("c.model", f.Models)
	addIn("u.provider", f.Providers)

	return " WHERE " + strings.Join(conds, " AND "), args
}

// SummarizeTurnContext aggregates what the turns that ran UNDER each value of
// ONE dimension actually cost, grouped per ActivityFilter.GroupBy (which accepts
// "value" and the queried dimension's own name, and refuses the call-level
// "kind"/"name"). This is the real answer to "which skill/agent is expensive":
// TopActivity ranks the turn that INVOKED a skill, which is one call, while the
// skill's own work is every turn that followed under it, and for a subagent
// there is no invoking call in the ledger at all.
//
// Unlike the activity queries it does NOT divide. Within one dimension each
// usage event has at most one context and the join is 1:1, so a bucket's cost is
// the full ledger cost of its turns and the buckets partition the window without
// overlap. It also counts turns that called no tool at all, which the activity
// ledger has no row for.
//
// THE DIMENSION IS A REQUIRED ARGUMENT, NOT A FILTER. The five dimensions plus
// activity_events' tool-call attribution are SIX PARTITIONS OF THE SAME DOLLARS:
// a turn commonly carries three or four contexts at once, each naming its full
// cost, so a query spanning two of them reports the same tokens twice. An
// unknown dimension is refused, grouping by "dimension" is refused, and grouping
// by a dimension OTHER than the queried one is refused — the mixing is
// unexpressible rather than discouraged.
func (s *Reader) SummarizeTurnContext(ctx context.Context, dim model.TurnDimension, f ActivityFilter) (*TurnContextSummary, error) {
	if err := checkTurnContextFilter(dim, f); err != nil {
		return nil, err
	}
	groupExprs, err := turnGroupExprs(dim, f)
	if err != nil {
		return nil, err
	}
	where, args := buildTurnContextWhere(dim, f)

	var sb strings.Builder
	sb.WriteString("SELECT ")
	for _, ge := range groupExprs {
		sb.WriteString(ge)
		sb.WriteString(", ")
	}
	sb.WriteString(turnContextSelectSQL)
	sb.WriteString(turnContextFromSQL)
	sb.WriteString(where)
	if len(groupExprs) > 0 {
		joined := strings.Join(groupExprs, ", ")
		sb.WriteString(" GROUP BY " + joined + " ORDER BY " + joined)
	}

	buckets, err := s.queryTurnContextBuckets(ctx, sb.String(), args, f.GroupBy)
	if err != nil {
		return nil, err
	}

	sum := &TurnContextSummary{
		Dimension: dim,
		GroupBy:   append([]string{}, f.GroupBy...),
		Buckets:   buckets,
	}

	// Grand total from the single pass, exactly as SummarizeActivity does it:
	// ungrouped, the one row IS the total; grouped, every field adds across
	// buckets except the distinct session count, which needs its own query.
	if len(f.GroupBy) == 0 {
		if len(buckets) == 1 {
			sum.Totals = buckets[0]
		}
		return sum, nil
	}
	for _, b := range buckets {
		sum.Totals.Turns += b.Turns
		sum.Totals.InputTokens += b.InputTokens
		sum.Totals.OutputTokens += b.OutputTokens
		sum.Totals.TotalTokens += b.TotalTokens
		sum.Totals.CostMicroUSD += b.CostMicroUSD
		sum.Totals.UnjoinedTurns += b.UnjoinedTurns
		sum.Totals.UnpricedTurns += b.UnpricedTurns
		sum.Totals.ComputedCostTurns += b.ComputedCostTurns
	}
	if len(buckets) > 0 {
		n, err := s.distinctTurnContextSessions(ctx, where, args)
		if err != nil {
			return nil, err
		}
		sum.Totals.Sessions = n
	}
	return sum, nil
}

// TopTurnContext ranks SummarizeTurnContext's buckets by one metric and caps them
// at limit (0 = uncapped), ordering and limiting in SQL — the real "which
// skill/agent/server is expensive" query. It needs at least one GroupBy
// dimension, takes the dimension on the same required-argument terms
// SummarizeTurnContext does, and ActivityByCalls ranks by turns here.
func (s *Reader) TopTurnContext(ctx context.Context, dim model.TurnDimension, f ActivityFilter, by ActivityOrder, limit int) ([]TurnContextBucket, error) {
	if err := checkTurnContextFilter(dim, f); err != nil {
		return nil, err
	}
	groupExprs, err := turnGroupExprs(dim, f)
	if err != nil {
		return nil, err
	}
	if len(groupExprs) == 0 {
		return nil, fmt.Errorf("store: TopTurnContext needs at least one group dimension to rank")
	}
	order, err := turnContextOrderExpr(by)
	if err != nil {
		return nil, err
	}
	where, args := buildTurnContextWhere(dim, f)

	joined := strings.Join(groupExprs, ", ")
	q := "SELECT " + joined + ", " + turnContextSelectSQL + turnContextFromSQL + where +
		" GROUP BY " + joined +
		// The group expressions break ties, so a ranking is deterministic
		// rather than at the mercy of scan order.
		" ORDER BY " + order + " DESC, " + joined
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return s.queryTurnContextBuckets(ctx, q, args, f.GroupBy)
}

// SummarizeSkillCost is SummarizeTurnContext pinned to the skill dimension. It
// is a delegation and not a second implementation: the skill partition is not a
// special case of anything, it is one of five, and giving it its own SQL would
// be the two-tables mistake wearing a function signature.
func (s *Reader) SummarizeSkillCost(ctx context.Context, f ActivityFilter) (*SkillCostSummary, error) {
	return s.SummarizeTurnContext(ctx, model.DimensionSkill, f)
}

// TopSkillCost is TopTurnContext pinned to the skill dimension. See
// SummarizeSkillCost.
func (s *Reader) TopSkillCost(ctx context.Context, f ActivityFilter, by ActivityOrder, limit int) ([]SkillCostBucket, error) {
	return s.TopTurnContext(ctx, model.DimensionSkill, f, by, limit)
}

// queryTurnContextBuckets runs a built turn-context query and scans its rows.
// Both public queries share it so their projections cannot drift apart.
func (s *Reader) queryTurnContextBuckets(ctx context.Context, q string, args []any, groupBy []string) ([]TurnContextBucket, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: summarize turn context: %w", err)
	}
	defer rows.Close()

	var out []TurnContextBucket
	for rows.Next() {
		keyVals := make([]string, len(groupBy))
		dest := make([]any, 0, len(groupBy)+8)
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var b TurnContextBucket
		dest = append(dest, &b.Turns, &b.Sessions,
			&b.InputTokens, &b.OutputTokens, &b.TotalTokens,
			&b.CostMicroUSD, &b.UnjoinedTurns, &b.UnpricedTurns, &b.ComputedCostTurns)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan turn context row: %w", err)
		}
		if len(groupBy) > 0 {
			b.Keys = make(map[string]string, len(groupBy))
			b.OrderedKeys = append([]string{}, groupBy...)
			for i, dim := range groupBy {
				b.Keys[dim] = keyVals[i]
			}
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: turn context rows: %w", err)
	}
	return out, nil
}

// distinctTurnContextSessions counts distinct non-empty session ids over the
// filtered context set.
func (s *Reader) distinctTurnContextSessions(ctx context.Context, where string, args []any) (int64, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN c.session_id <> '' THEN c.session_id END)`+
		turnContextFromSQL+where, args...)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: distinct turn context sessions: %w", err)
	}
	return n, nil
}

// insertTurnContextsTx appends turn-context rows inside an existing transaction,
// with the same idempotence the two ledgers get: ON CONFLICT(usage_dedup_key,
// dimension) DO NOTHING keeps a re-read silent while a CHECK violation (empty
// key, unknown dimension, empty value) still errors, which a blanket OR IGNORE
// would swallow. Only CHECK failures and empty keys are skipped and summarised
// in skipErr; every other insert error fails the batch.
//
// The conflict target is (the usage row, the axis), which is what makes a second
// sighting of the same turn a no-op rather than a second helping of its cost,
// while still letting the same turn record a value on each of the other axes.
func insertTurnContextsTx(ctx context.Context, tx *sql.Tx, ctxs []model.TurnContext) (inserted int, skipErr, err error) {
	if len(ctxs) == 0 {
		return 0, nil, nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO usage_turn_context (
			usage_dedup_key, dimension, value, tool, session_id, project, model,
			event_time_unix, observed_time_unix, source_path
		) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(usage_dedup_key, dimension) DO NOTHING`)
	if err != nil {
		return 0, nil, fmt.Errorf("store: prepare turn context insert: %w", err)
	}
	defer stmt.Close()

	skips := rowSkips{table: tableTurnContext}
	for _, c := range ctxs {
		if err := ctx.Err(); err != nil {
			return inserted, nil, err
		}
		if c.UsageDedupKey == "" {
			skips.add(c.UsageDedupKey, fmt.Errorf("store: turn context with empty usage dedup key (tool=%s dimension=%s value=%s)", c.Tool, c.Dimension, c.Value))
			continue
		}
		obs := c.ObservedTime
		if obs.IsZero() {
			obs = c.EventTime
		}
		res, execErr := stmt.ExecContext(ctx,
			c.UsageDedupKey, string(c.Dimension), c.Value, c.Tool, c.SessionID, c.Project, c.Model,
			c.EventTime.UTC().Unix(), obs.UTC().Unix(), c.SourcePath,
		)
		if execErr != nil {
			rowErr := fmt.Errorf("store: insert turn context %s/%s: %w", c.UsageDedupKey, c.Dimension, execErr)
			if !isSkippableRowError(execErr) {
				return inserted, nil, rowErr
			}
			skips.add(c.UsageDedupKey, rowErr)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, skips.err(len(ctxs)), nil
}
