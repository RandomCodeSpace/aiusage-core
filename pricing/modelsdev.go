package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

//go:embed data/modelsdev_snapshot.json
var modelsDevSnapshotJSON []byte

//go:embed data/modelsdev_LICENSE
var modelsDevLicense string

type modelsDevDoc struct {
	Meta struct {
		Source  string `json:"source"`
		Fetched string `json:"fetched"`
		License string `json:"license"`
	} `json:"_meta"`
	Providers map[string]modelsDevProvider `json:"providers"`
}

type modelsDevProvider struct {
	ID     string                     `json:"id,omitempty"`
	Models map[string]json.RawMessage `json:"models"`
}

// Models.dev publishes USD per million tokens. Keep those source units in the
// snapshot; conversion to the engine's USD per token happens only in rates.
type modelsDevCard struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

type modelsDevTier struct {
	modelsDevCard
	Tier struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

type modelsDevCost struct {
	modelsDevCard
	Tiers []modelsDevTier `json:"tiers,omitempty"`
}

func (c modelsDevCost) rates() Rates {
	r := Rates{Input: c.Input / 1e6, Output: c.Output / 1e6,
		CacheRead: c.CacheRead / 1e6, CacheWrite5m: c.CacheWrite / 1e6}
	if len(c.Tiers) == 1 {
		t := c.Tiers[0]
		r.Long = LongContext{Threshold: t.Tier.Size, Input: t.Input / 1e6,
			Output: t.Output / 1e6, CacheRead: t.CacheRead / 1e6,
			CacheWrite5m: t.CacheWrite / 1e6}
	}
	return r
}

// modelsDevCardFrom validates every published price before filtering. Missing
// or null input/output is unknown. A separate reasoning price cannot be
// represented by Rates, whose reasoning bucket uses the output price.
func modelsDevCardFrom(fields map[string]json.RawMessage) (modelsDevCard, bool) {
	values := make(map[string]float64, len(fields))
	for key, raw := range fields {
		switch key {
		case "input", "output", "cache_read", "cache_write", "reasoning", "input_audio", "output_audio":
		default:
			return modelsDevCard{}, false
		}
		var value *float64
		if json.Unmarshal(raw, &value) != nil || value == nil || *value < 0 || math.IsInf(*value, 0) || math.IsNaN(*value) {
			return modelsDevCard{}, false
		}
		values[key] = *value
	}
	in, hasInput := values["input"]
	out, hasOutput := values["output"]
	if !hasInput || !hasOutput {
		return modelsDevCard{}, false
	}
	if reasoning, ok := values["reasoning"]; ok && reasoning != out {
		return modelsDevCard{}, false
	}
	for _, key := range []string{"cache_read", "cache_write"} {
		if value, published := values[key]; published && value == 0 && in > 0 {
			// Rates uses zero for an absent cache rate and falls back to paid
			// input. It cannot represent an explicitly free cache bucket.
			return modelsDevCard{}, false
		}
	}
	if in == 0 && out == 0 {
		for _, value := range values {
			if value > 0 {
				// Do not turn positive audio or other prices into a free offer
				// by dropping their unsupported fields during normalization.
				return modelsDevCard{}, false
			}
		}
	}
	return modelsDevCard{Input: in, Output: out, CacheRead: values["cache_read"], CacheWrite: values["cache_write"]}, true
}

func modelsDevCostFrom(raw json.RawMessage) (modelsDevCost, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return modelsDevCost{}, false
	}
	tiers, hasTiers := fields["tiers"]
	legacy, hasLegacy := fields["context_over_200k"]
	delete(fields, "tiers")
	delete(fields, "context_over_200k")
	card, ok := modelsDevCardFrom(fields)
	if !ok {
		return modelsDevCost{}, false
	}
	cost := modelsDevCost{modelsDevCard: card}
	if hasTiers {
		var entries []map[string]json.RawMessage
		if json.Unmarshal(tiers, &entries) != nil || entries == nil || len(entries) > 1 {
			return modelsDevCost{}, false
		}
		if len(entries) == 0 {
			return cost, !hasLegacy
		}
		fields = entries[0]
		var tier modelsDevTier
		if json.Unmarshal(fields["tier"], &tier.Tier) != nil || tier.Tier.Type != "context" || tier.Tier.Size <= 0 {
			return modelsDevCost{}, false
		}
		delete(fields, "tier")
		tier.modelsDevCard, ok = modelsDevCardFrom(fields)
		if !ok {
			return modelsDevCost{}, false
		}
		// Upstream emits context_over_200k even for larger thresholds. The
		// explicit tier is authoritative; never turn its 272K into 200K.
		cost.Tiers = []modelsDevTier{tier}
	} else if hasLegacy {
		fields = nil
		if json.Unmarshal(legacy, &fields) != nil {
			return modelsDevCost{}, false
		}
		card, ok := modelsDevCardFrom(fields)
		if !ok {
			return modelsDevCost{}, false
		}
		tier := modelsDevTier{modelsDevCard: card}
		tier.Tier.Type, tier.Tier.Size = "context", 200_000
		cost.Tiers = []modelsDevTier{tier}
	}
	if len(cost.Tiers) == 1 {
		tier := cost.Tiers[0]
		// LongContext uses zero to inherit a base rate, so a published
		// reduction to free cannot be represented by that card.
		if (tier.Input == 0 && cost.Input > 0) || (tier.Output == 0 && cost.Output > 0) {
			return modelsDevCost{}, false
		}
		if _, published := fields["cache_read"]; published && tier.CacheRead == 0 && (cost.CacheRead > 0 || cost.Input > 0) {
			return modelsDevCost{}, false
		}
		if _, published := fields["cache_write"]; published && tier.CacheWrite == 0 && (cost.CacheWrite > 0 || cost.Input > 0) {
			return modelsDevCost{}, false
		}
	}
	return cost, true
}

// filterModelsDev is shared by the cache/embedded decoder and snapshot builder.
// A bad model must not discard good neighbors. A feed with none left is an
// error, so an unsuccessful refresh cannot replace a working table with empty.
func filterModelsDev(providers map[string]modelsDevProvider) (map[string]modelsDevProvider, map[string]Rates, error) {
	kept := make(map[string]modelsDevProvider)
	models := make(map[string]Rates)
	for provider, data := range providers {
		if provider == "" || provider != strings.TrimSpace(provider) || strings.Contains(provider, "/") || (data.ID != "" && data.ID != provider) {
			continue
		}
		entries := make(map[string]json.RawMessage)
		for name, raw := range data.Models {
			if name == "" || name != strings.TrimSpace(name) {
				continue
			}
			var model struct {
				ID   string          `json:"id"`
				Cost json.RawMessage `json:"cost"`
			}
			if json.Unmarshal(raw, &model) != nil || (model.ID != "" && model.ID != name) {
				continue
			}
			cost, ok := modelsDevCostFrom(model.Cost)
			if !ok {
				continue
			}
			rates := cost.rates()
			// Zero is frequently a placeholder in this feed. These exact
			// OpenCode offers are the only confirmed free exceptions.
			rates.Free = provider == "opencode" && (name == "hy3-free" || name == "big-pickle") &&
				cost.modelsDevCard == (modelsDevCard{}) && len(cost.Tiers) == 0
			if !rates.Free && !rates.Priceable() {
				continue
			}
			encoded, err := json.Marshal(struct {
				Cost modelsDevCost `json:"cost"`
			}{cost})
			if err != nil {
				return nil, nil, fmt.Errorf("pricing: encode Models.dev model: %w", err)
			}
			entries[name] = encoded
			models[provider+"/"+name] = rates
		}
		if len(entries) > 0 {
			kept[provider] = modelsDevProvider{Models: entries}
		}
	}
	if len(models) == 0 {
		return nil, nil, fmt.Errorf("pricing: Models.dev table has no priceable models")
	}
	return kept, models, nil
}

func decodeModelsDevSnapshot(data []byte, prefix string) (*Table, error) {
	var doc modelsDevDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("pricing: parse Models.dev snapshot: %w", err)
	}
	_, models, err := filterModelsDev(doc.Providers)
	if err != nil {
		return nil, err
	}
	date := doc.Meta.Fetched
	if date == "" {
		date = "unknown"
	} else if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, fmt.Errorf("pricing: invalid Models.dev fetch date: %w", err)
	}
	return &Table{Source: prefix + "-" + date, Models: models, ProviderScoped: true}, nil
}

func modelsDevSnapshot(data []byte) ([]byte, error) {
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("pricing: parse Models.dev data: %w", err)
	}
	kept, _, err := filterModelsDev(providers)
	if err != nil {
		return nil, err
	}
	doc := modelsDevDoc{Providers: kept}
	doc.Meta.Source = ModelsDevURL
	doc.Meta.License = modelsDevLicense
	doc.Meta.Fetched = nowFn().UTC().Format("2006-01-02")
	return json.MarshalIndent(doc, "", " ")
}

// BuildModelsDevSnapshot filters Models.dev API data into the same envelope
// consumed by the runtime cache. Used by the offline snapshot generator.
func BuildModelsDevSnapshot(data []byte) ([]byte, error) { return modelsDevSnapshot(data) }

var embeddedModelsDevTable = sync.OnceValue(func() *Table {
	table, err := decodeModelsDevSnapshot(modelsDevSnapshotJSON, "embedded-modelsdev")
	if err != nil {
		return nil
	}
	return table
})
