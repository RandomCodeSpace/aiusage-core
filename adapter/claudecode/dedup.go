package claudecode

import "github.com/RandomCodeSpace/aiusage-core/model"

// deduper implements ccusage-equivalent in-cycle deduplication for Claude Code
// transcript lines.
//
// Rules:
//   - Lines without a message id are never deduped (kept verbatim).
//   - Primary identity is (messageID, requestID).
//   - A sidechain replay shares the messageID but may carry a different
//     requestID; such a secondary collision is consolidated only when either
//     the resident or the incoming candidate isSidechain.
//   - On any collision the better candidate wins (keep-best ordering):
//     non-sidechain > higher token total > higher cost > has speed.
//
// USAGE COLLAPSES, ACTIVITY UNIONS. The two streams take opposite treatments of
// the same collision and the difference is not a subtlety, it is the whole
// correctness of the activity ledger. One API response is streamed across
// several transcript records sharing a message id; they repeat the response's
// token counts, so exactly one may become a usage row — keep-best. They do NOT
// repeat the response's tool_use blocks, they PARTITION them, so every record's
// blocks belong to the surviving turn. Taking only the winner's blocks (which
// is what this did until the union below) silently dropped 19,682 of 60,832
// local calls, and non-uniformly: Read lost 45% of its calls, Bash 29%, so
// rankings were distorted rather than merely low.
type deduper struct {
	primary   map[primaryKey]*candidate // (messageID, requestID) -> winner
	byMessage map[string]primaryKey     // messageID -> resident primary key
	// calls is the UNION of the tool calls seen under a primary key, in
	// first-seen order, deduplicated on toolCall.identity so a sidechain replay
	// of an already-seen record contributes nothing. Its length is the divisor
	// the read path attributes cost with, so it has to be the count across every
	// record of the message rather than any one record's.
	calls     map[primaryKey][]toolCall
	seenCalls map[primaryKey]map[string]struct{}
	// contexts is the UNION of the turn attributions seen under a primary key,
	// in first-seen dimension order, at most one value per dimension. It exists
	// for the same reason calls does and is the same fix: see addContexts.
	contexts map[primaryKey][]turnContext
	seenDims map[primaryKey]map[model.TurnDimension]struct{}
	noID     []candidate  // never-deduped (no message id)
	order    []primaryKey // stable emission order for deduped ones
	// hooks are activity rows that belong to no usage candidate: hook records
	// carry no message id and no usage block, so none of the dedup rules above
	// apply to them. Their own dedup keys (the record uuid) make a re-read
	// idempotent at the store instead.
	hooks []model.ActivityEvent
}

type primaryKey struct {
	messageID string
	requestID string
}

func newDeduper() *deduper {
	return &deduper{
		primary:   make(map[primaryKey]*candidate),
		byMessage: make(map[string]primaryKey),
		calls:     make(map[primaryKey][]toolCall),
		seenCalls: make(map[primaryKey]map[string]struct{}),
		contexts:  make(map[primaryKey][]turnContext),
		seenDims:  make(map[primaryKey]map[model.TurnDimension]struct{}),
	}
}

// add ingests one candidate, applying the dedup rules. Whichever branch it
// takes, the candidate's tool calls join the union of the primary key its usage
// event ends up under — including the branches where the candidate itself loses
// and is discarded.
func (d *deduper) add(c candidate) {
	if c.messageID == "" {
		d.noID = append(d.noID, c)
		return
	}

	pk := primaryKey{messageID: c.messageID, requestID: c.requestID}

	// Primary collision: same message id AND request id. This is the streaming
	// case — locally the ONLY one: no message id anywhere in 1,300 transcripts
	// carries two request ids.
	if existing, ok := d.primary[pk]; ok {
		d.addCalls(pk, c.calls)
		d.addContexts(pk, c.contexts)
		if better(c, *existing) {
			cp := c
			d.primary[pk] = &cp
		}
		return
	}

	// Secondary (sidechain-replay) collision: same message id, different
	// request id. Consolidate only when a sidechain is involved on either side.
	if residentKey, ok := d.byMessage[c.messageID]; ok {
		resident := d.primary[residentKey]
		if resident != nil && (resident.isSidechain || c.isSidechain) {
			d.addCalls(residentKey, c.calls)
			d.addContexts(residentKey, c.contexts)
			if better(c, *resident) {
				// Replace the resident in place under its existing primary key
				// so emission order is preserved.
				cp := c
				d.primary[residentKey] = &cp
			}
			return
		}
	}

	// New distinct record.
	cp := c
	d.primary[pk] = &cp
	d.byMessage[c.messageID] = pk
	d.order = append(d.order, pk)
	d.addCalls(pk, c.calls)
	d.addContexts(pk, c.contexts)
}

// addCalls appends calls not already present under pk, preserving first-seen
// order. A sidechain replay repeats a record's tool_use blocks verbatim, so the
// identity check is what keeps a replayed call from inflating the divisor.
func (d *deduper) addCalls(pk primaryKey, calls []toolCall) {
	if len(calls) == 0 {
		return
	}
	seen, ok := d.seenCalls[pk]
	if !ok {
		seen = make(map[string]struct{}, len(calls))
		d.seenCalls[pk] = seen
	}
	for _, c := range calls {
		id := c.identity()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		d.calls[pk] = append(d.calls[pk], c)
	}
}

// addContexts merges one record's turn attributions into the union under pk,
// FIRST NON-EMPTY VALUE WINS PER DIMENSION.
//
// THE UNION IS THE POINT, AND IT IS THE NAMED BUG CLASS. One usage identity
// spans several transcript records; exactly one of them survives as the usage
// row, and `better` picks that one on token total, cost and the presence of a
// speed field — metrics with no relationship whatsoever to attribution. Reading
// the attribution off the WINNER means a winner that happens not to carry the
// field silently discards a fact its losing siblings recorded, which is
// precisely how the losing candidates' tool calls went missing: 19,682 of 60,832
// calls, non-uniformly, until this same union landed for calls. Taking the union
// makes the outcome independent of which record wins, so the bug is not fixed
// here, it is not expressible here.
//
// WHAT THE SOURCE ACTUALLY DOES, measured over 1,275 local transcripts covering
// 102,887 usage-bearing assistant records: the records of a message never
// disagree and never partially agree. For every dimension, of the message ids
// carrying it (agent 40,433, skill 3,009, mcp_tool 3,265, mcp_server 3,265,
// plugin 1,317) EVERY record of that message carries it, and every record of
// that message carries the SAME value — zero disagreements, zero partial
// presence, on all five. So today the union and the winner's copy agree on every
// row in the corpus.
//
// That measurement is a reason to be sure the rule is cheap, not a reason to
// skip it. It describes what one version of one CLI happened to write; it is not
// a guarantee the format offers, and the failure mode if it ever changes is
// silent under-attribution that looks exactly like "that skill was cheap". The
// tie-break, should the source ever disagree, is FIRST SEEN — deterministic
// across passes because files are walked in lexical order and lines in file
// order, so a re-read produces the same row rather than flapping between two.
func (d *deduper) addContexts(pk primaryKey, ctxs []turnContext) {
	if len(ctxs) == 0 {
		return
	}
	seen, ok := d.seenDims[pk]
	if !ok {
		seen = make(map[model.TurnDimension]struct{}, len(ctxs))
		d.seenDims[pk] = seen
	}
	for _, c := range ctxs {
		if _, dup := seen[c.dim]; dup {
			continue
		}
		seen[c.dim] = struct{}{}
		d.contexts[pk] = append(d.contexts[pk], c)
	}
}

// better reports whether candidate a should win over b under the keep-best
// ordering: non-sidechain beats sidechain, then higher token total, then higher
// cost, then presence of a speed field.
func better(a, b candidate) bool {
	if a.isSidechain != b.isSidechain {
		return !a.isSidechain // non-sidechain wins
	}
	if a.total != b.total {
		return a.total > b.total
	}
	if a.cost != b.cost {
		return a.cost > b.cost
	}
	if a.hasSpeed != b.hasSpeed {
		return a.hasSpeed
	}
	return false
}

// addHooks records activity rows that stand outside the usage dedup rules.
func (d *deduper) addHooks(a []model.ActivityEvent) {
	d.hooks = append(d.hooks, a...)
}

// events returns the surviving usage events: deduped records in first-seen
// order followed by never-deduped (no-id) records in file order.
func (d *deduper) events() []model.UsageEvent {
	out := make([]model.UsageEvent, 0, len(d.order)+len(d.noID))
	for _, pk := range d.order {
		if c := d.primary[pk]; c != nil {
			out = append(out, c.event)
		}
	}
	for _, c := range d.noID {
		out = append(out, c.event)
	}
	return out
}

// activity returns the activity rows of every surviving turn, in the order
// events() emits those turns, plus the hook rows.
//
// The rows come from the union of the turn's records but are ATTRIBUTED to the
// winning candidate's usage event — its dedup key, its timestamp, its session
// and project. That pairing is the point: the union makes the divisor equal the
// number of rows the key will have, and the winner is the only record of the
// message that becomes a usage row, so every call names a key the ledger
// actually holds.
func (d *deduper) activity() []model.ActivityEvent {
	out := make([]model.ActivityEvent, 0, len(d.hooks))
	for _, pk := range d.order {
		if c := d.primary[pk]; c != nil {
			out = append(out, mintActivity(d.calls[pk], c.event, c.requestID)...)
		}
	}
	// A record with no message id is never deduped, so it is its own turn and
	// its own record's calls are the whole union.
	for i := range d.noID {
		c := &d.noID[i]
		out = append(out, mintActivity(c.calls, c.event, c.requestID)...)
	}
	return append(out, d.hooks...)
}

// turnContexts returns one row per (surviving usage event, dimension) the
// transcripts attributed, taken from the same winners events() emits and in the
// same order.
//
// The VALUES come from the union across every record of the message
// (addContexts); the usage event they are attributed to is the WINNER's, because
// that is the only record of the message that becomes a usage row and a context
// naming any other key would join nothing. That pairing — union for the fact,
// winner for the key — is the same one activity() uses, and for the same reason.
//
// A turn contributes one row PER DIMENSION and each names the turn's full cost,
// so these rows are partitions rather than shares: five rows for one turn is
// five complete answers to five different questions, not five fifths of
// anything. Hooks contribute none — they carry no usage object to attribute.
func (d *deduper) turnContexts() []model.TurnContext {
	var out []model.TurnContext
	add := func(c *candidate, ctxs []turnContext) {
		if c == nil {
			return
		}
		for _, tc := range ctxs {
			out = append(out, model.TurnContext{
				UsageDedupKey: c.event.DedupKey,
				Tool:          model.ToolClaudeCode,
				Dimension:     tc.dim,
				Value:         tc.value,
				SessionID:     c.event.SessionID,
				Project:       c.event.Project,
				Model:         c.event.Model,
				EventTime:     c.event.EventTime,
				SourcePath:    c.event.SourcePath,
			})
		}
	}
	for _, pk := range d.order {
		add(d.primary[pk], d.contexts[pk])
	}
	// A record with no message id is never deduped, so it is its own turn and
	// its own record's attributions are the whole union.
	for i := range d.noID {
		c := &d.noID[i]
		add(c, c.contexts)
	}
	return out
}
