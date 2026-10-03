package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"sync"
)

// snapshotJSON is the filtered LiteLLM price snapshot that ships in the binary.
// It is the guaranteed floor of the ladder: an install with no network and no
// config still prices the models this project ingests. Regenerate it from
// upstream (see data/litellm_snapshot.json's _meta block) rather than editing
// rates by hand.
//
//go:embed data/litellm_snapshot.json
var snapshotJSON []byte

// litellmEntry mirrors the price fields of LiteLLM's
// model_prices_and_context_window.json. The embedded snapshot keeps upstream's
// field names verbatim so one decoder serves both the snapshot and the file
// fetched at runtime.
type litellmEntry struct {
	Input        float64 `json:"input_cost_per_token"`
	Output       float64 `json:"output_cost_per_token"`
	CacheRead    float64 `json:"cache_read_input_token_cost"`
	CacheWrite   float64 `json:"cache_creation_input_token_cost"`
	CacheWrite1h float64 `json:"cache_creation_input_token_cost_above_1hr"`
	InputBatch   float64 `json:"input_cost_per_token_batches"`
	OutputBatch  float64 `json:"output_cost_per_token_batches"`

	// Long carries the above-threshold rate card, whose LiteLLM keys embed the
	// threshold in the field NAME and so cannot be struct tags. UnmarshalJSON
	// fills it; the tag keeps encoding/json from looking for a "Long" key.
	Long LongContext `json:"-"`
}

// litellmFixedFields are the price keys litellmEntry names as struct tags, i.e.
// every field the decoder reads whose name does not carry a threshold. It exists
// so SnapshotField can answer for the snapshot generator without a second,
// drifting copy of the list; TestLitellmFixedFieldsMatchTags cross-checks it
// against the tags themselves.
var litellmFixedFields = map[string]bool{
	"input_cost_per_token":                      true,
	"output_cost_per_token":                     true,
	"cache_read_input_token_cost":               true,
	"cache_creation_input_token_cost":           true,
	"cache_creation_input_token_cost_above_1hr": true,
	"input_cost_per_token_batches":              true,
	"output_cost_per_token_batches":             true,
}

// longContextRateField matches the LiteLLM keys that publish an above-threshold
// rate for a bucket this package bills, and captures the boundary out of the
// name. LiteLLM's full grammar is
// "<bucket>[_above_1hr]_above_<N>k_tokens[_priority|_flex]"; this pattern is
// anchored at both ends, which decides the two questions the grammar raises:
//
//   - the 1h cache write CROSSED with long context
//     ("cache_creation_input_token_cost_above_1hr_above_200k_tokens", 10 models)
//     is kept, because aiusage already models the 1h write and would otherwise
//     bill it off the short card inside a long request;
//   - "_priority" and "_flex" are DELIBERATELY dropped. They are service tiers
//     this package does not model at all (isBatchTier is the only tier branch),
//     so reading one as the long-context rate would price a standard request off
//     a priority card. Skipping them is a decision, not an oversight: when a
//     service tier for them arrives, it gets its own fields.
//
// Non-token units ("input_cost_per_character_above_128k_tokens",
// "..._per_image_...", "..._per_video_per_second_...") are excluded by naming the
// four token buckets explicitly rather than matching a prefix.
var longContextRateField = regexp.MustCompile(
	`^(input_cost_per_token|output_cost_per_token|cache_read_input_token_cost|cache_creation_input_token_cost|cache_creation_input_token_cost_above_1hr)_above_([0-9]+)k_tokens$`)

// SnapshotField reports whether a LiteLLM price key is one this decoder reads,
// i.e. one the embedded snapshot must KEEP. It is exported for the snapshot
// generator (gensnapshot.go), which is the only caller: the generator's filter
// stripping a field the decoder reads is exactly the bug that left every
// air-gapped install flat-pricing long-context turns, and one predicate for both
// halves makes the two unable to disagree.
func SnapshotField(key string) bool {
	return litellmFixedFields[key] || longContextRateField.MatchString(key)
}

// UnmarshalJSON decodes the fixed-name price fields by tag and the
// threshold-bearing ones by pattern. Both halves read the SAME object, so a
// snapshot and a raw upstream entry decode identically.
func (e *litellmEntry) UnmarshalJSON(data []byte) error {
	// plain drops the method set, so decoding it does not recurse.
	type plain litellmEntry
	var fixed plain
	if err := json.Unmarshal(data, &fixed); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*e = litellmEntry(fixed)
	e.Long = longContextCard(fields)
	return nil
}

// longContextCard assembles the above-threshold rate card from an entry's raw
// fields. No model in the live table publishes rates at two different boundaries
// (verified: 120 tiered models, five distinct thresholds — 128K, 200K, 256K,
// 272K, 512K — none carrying more than one), so a single card per model is the
// whole data model. Should upstream ever disagree with itself, the LOWEST
// boundary wins and only the rates published AT that boundary are used: a card
// blended across two boundaries would price both of them wrong, and picking by
// map iteration order would price the same table differently on every run.
func longContextCard(fields map[string]json.RawMessage) LongContext {
	cards := make(map[int64]LongContext)
	for key, raw := range fields {
		m := longContextRateField.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		k, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || k <= 0 {
			continue
		}
		var rate float64
		if err := json.Unmarshal(raw, &rate); err != nil || rate <= 0 {
			continue
		}
		threshold := k * 1000
		card := cards[threshold]
		card.Threshold = threshold
		switch m[1] {
		case "input_cost_per_token":
			card.Input = rate
		case "output_cost_per_token":
			card.Output = rate
		case "cache_read_input_token_cost":
			card.CacheRead = rate
		case "cache_creation_input_token_cost":
			card.CacheWrite5m = rate
		case "cache_creation_input_token_cost_above_1hr":
			card.CacheWrite1h = rate
		}
		cards[threshold] = card
	}
	var out LongContext
	for threshold, card := range cards {
		if out.Threshold == 0 || threshold < out.Threshold {
			out = card
		}
	}
	return out
}

func (e litellmEntry) rates() Rates {
	return Rates{
		Input:        e.Input,
		Output:       e.Output,
		CacheRead:    e.CacheRead,
		CacheWrite5m: e.CacheWrite,
		CacheWrite1h: e.CacheWrite1h,
		InputBatch:   e.InputBatch,
		OutputBatch:  e.OutputBatch,
		Long:         e.Long,
	}
}

// snapshotDoc is the envelope used by both the embedded snapshot and the
// runtime cache: provenance plus the model map. Upstream's own file is a bare
// model map, which decodeModels reads directly.
type snapshotDoc struct {
	Meta struct {
		Source  string `json:"source"`
		Fetched string `json:"fetched"`
	} `json:"_meta"`
	Models map[string]json.RawMessage `json:"models"`
}

// decodeModels turns a raw model map into rates. Entries that do not decode, or
// that publish no usable price, are skipped instead of failing the table: the
// refresh path consumes a file this project does not control, and one odd
// record must not cost the user every price.
func decodeModels(raw map[string]json.RawMessage) map[string]Rates {
	out := make(map[string]Rates, len(raw))
	for name, msg := range raw {
		var e litellmEntry
		if err := json.Unmarshal(msg, &e); err != nil {
			continue
		}
		r := e.rates()
		if !r.Priceable() {
			continue
		}
		out[name] = r
	}
	return out
}

// decodeUpstream parses LiteLLM's own bare model map, as served by the refresh
// URL.
func decodeUpstream(data []byte) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pricing: parse upstream price table: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("pricing: upstream price table is empty")
	}
	return raw, nil
}

// decodeSnapshot parses the envelope form (embedded snapshot or runtime cache)
// into a table stamped with the given price_source prefix.
func decodeSnapshot(data []byte, prefix string) (*Table, error) {
	var doc snapshotDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("pricing: parse price snapshot: %w", err)
	}
	models := decodeModels(doc.Models)
	if len(models) == 0 {
		return nil, fmt.Errorf("pricing: price snapshot has no priceable models")
	}
	date := doc.Meta.Fetched
	if date == "" {
		date = "unknown"
	}
	return &Table{Source: prefix + "-" + date, Models: models}, nil
}

// embeddedTable parses the embedded snapshot once. A parse failure can only
// mean a corrupted embed, in which case the rung is simply absent and the
// ladder falls through — pricing must never stop the program from running.
var embeddedTable = sync.OnceValue(func() *Table {
	t, err := decodeSnapshot(snapshotJSON, "embedded")
	if err != nil {
		return nil
	}
	return t
})
