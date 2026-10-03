//go:build ignore

// Command gensnapshot regenerates the go:embed'ed LiteLLM price snapshot, the
// bottom rung of the pricing ladder and the only one an air-gapped install has.
//
// Run it from this directory:
//
//	go run gensnapshot.go                       # fetch pricing.RefreshURL
//	go run gensnapshot.go -from prices.json     # use a table already on disk
//	go run gensnapshot.go -modelsdev            # fetch Models.dev prices
//	go run gensnapshot.go -modelsdev -from api.json # use a saved Models.dev API response
//
// It keeps upstream's field NAMES verbatim, so one decoder serves the snapshot
// and the runtime-refreshed table alike, and it decides which fields survive
// with pricing.SnapshotField — the decoder's own predicate. That shared
// predicate is the point of the program: the previous snapshot was cut by a
// filter that knew nothing about "*_above_<N>k_tokens", so every long-context
// rate was stripped on the way in and an install with no network flat-priced
// requests the table did publish a second rate card for.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/pricing"
)

// filterDesc is recorded in _meta so the snapshot documents how it was cut. It
// describes the MODEL filter only; the field filter is pricing.SnapshotField.
const filterDesc = "litellm_provider in (anthropic, openai, gemini, github_copilot, " +
	"vertex_ai-anthropic_models, vertex_ai-language-models); mode in (chat, responses, completion); " +
	"nonzero input or output rate"

var providers = map[string]bool{
	"anthropic":                  true,
	"openai":                     true,
	"gemini":                     true,
	"github_copilot":             true,
	"vertex_ai-anthropic_models": true,
	"vertex_ai-language-models":  true,
}

var modes = map[string]bool{"chat": true, "responses": true, "completion": true}

type meta struct {
	Source  string `json:"source"`
	License string `json:"license"`
	Fetched string `json:"fetched"`
	Models  int    `json:"models"`
	Filter  string `json:"filter"`
}

type snapshot struct {
	Meta   meta                          `json:"_meta"`
	Models map[string]map[string]float64 `json:"models"`
}

func main() {
	from := flag.String("from", "", "read the table from this file instead of fetching it "+
		"(upstream's bare model map or aiusage's own cached envelope)")
	out := flag.String("out", "", "snapshot to write (defaults to data/<feed>_snapshot.json)")
	modelsdev := flag.Bool("modelsdev", false, "generate the Models.dev snapshot")
	flag.Parse()

	url, filename := pricing.RefreshURL, "litellm_snapshot.json"
	if *modelsdev {
		url, filename = pricing.ModelsDevURL, "modelsdev_snapshot.json"
	}
	if *out == "" {
		*out = filepath.Join("data", filename)
	}
	data, source, err := load(*from, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gensnapshot:", err)
		os.Exit(1)
	}
	var encoded []byte
	var detail string
	if *modelsdev {
		encoded, err = pricing.BuildModelsDevSnapshot(data)
		detail = fmt.Sprintf("%d bytes", len(encoded))
	} else {
		var snap snapshot
		snap, err = build(data, source)
		if err == nil {
			encoded, err = json.MarshalIndent(snap, "", " ")
			detail = fmt.Sprintf("%d models, fetched %s", snap.Meta.Models, snap.Meta.Fetched)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gensnapshot:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(encoded, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gensnapshot:", err)
		os.Exit(1)
	}
	fmt.Printf("%s: %s\n", *out, detail)
}

// load returns the raw table plus the source string to record in _meta.
func load(path, url string) ([]byte, string, error) {
	if path == "" {
		resp, err := http.Get(url)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		return data, url, err
	}
	data, err := os.ReadFile(path)
	return data, url, err
}

// build cuts the snapshot: upstream's bare model map or aiusage's cached
// envelope in, the filtered model set with only the decoder's fields out.
func build(data []byte, source string) (snapshot, error) {
	raw, fetched, err := models(data)
	if err != nil {
		return snapshot{}, err
	}
	snap := snapshot{
		Meta:   meta{Source: source, License: "MIT (BerriAI/litellm)", Fetched: fetched, Filter: filterDesc},
		Models: make(map[string]map[string]float64, 256),
	}
	for name, msg := range raw {
		var head struct {
			Provider string `json:"litellm_provider"`
			Mode     string `json:"mode"`
		}
		if err := json.Unmarshal(msg, &head); err != nil {
			continue // a non-object record ("sample_spec"), not a model
		}
		if !providers[head.Provider] || !modes[head.Mode] {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(msg, &fields); err != nil {
			continue
		}
		kept := make(map[string]float64, 8)
		for key, val := range fields {
			if !pricing.SnapshotField(key) {
				continue
			}
			var rate float64
			if err := json.Unmarshal(val, &rate); err != nil {
				continue
			}
			kept[key] = rate
		}
		if kept["input_cost_per_token"] == 0 && kept["output_cost_per_token"] == 0 {
			continue // no usable price: the ladder would treat it as a miss anyway
		}
		snap.Models[name] = kept
	}
	if len(snap.Models) == 0 {
		return snapshot{}, fmt.Errorf("filter kept no models")
	}
	snap.Meta.Models = len(snap.Models)
	return snap, nil
}

// models unwraps either shape of input: upstream's bare model map, or the
// envelope aiusage writes to its own cache (which also dates the fetch).
func models(data []byte) (map[string]json.RawMessage, string, error) {
	var doc struct {
		Meta struct {
			Fetched string `json:"fetched"`
		} `json:"_meta"`
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err == nil && len(doc.Models) > 0 {
		fetched := doc.Meta.Fetched
		if fetched == "" {
			fetched = time.Now().UTC().Format("2006-01-02")
		}
		return doc.Models, fetched, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", fmt.Errorf("parse price table: %w", err)
	}
	if len(raw) == 0 {
		return nil, "", fmt.Errorf("price table is empty")
	}
	return raw, time.Now().UTC().Format("2006-01-02"), nil
}
