// Package agy implements an AGGREGATE adapter for the Antigravity CLI.
//
// Antigravity 1.1.22 reports authoritative cumulative conversation usage in
// print-mode stream JSON result records. The terminal step_update for the same
// turn repeats incremental usage, so it is deliberately ignored. Captured
// result streams may be placed in an Antigravity data root (or an explicit
// adapter override) for collection. The earlier Gemini-shaped JSON parser is
// retained for source compatibility with existing captures.
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY and never written,
// locked, or modified.
package agy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/internal/geminishape"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// metaProject labels every Antigravity turn (no cwd is recorded here).
const metaProject = "agy"

// shape is the shared Gemini-telemetry parser stamped for this adapter.
var shape = geminishape.Shape{Tool: model.ToolAgy, Provider: model.ProviderGoogle, Project: metaProject}

const maxStreamRecordBytes = 8 << 20

type streamUsage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

type streamUsageRecord struct {
	InputTokens     *int64 `json:"input_tokens"`
	OutputTokens    *int64 `json:"output_tokens"`
	ThinkingTokens  *int64 `json:"thinking_tokens"`
	CacheReadTokens *int64 `json:"cache_read_tokens"`
	TotalTokens     *int64 `json:"total_tokens"`
}

type streamRecord struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	Init           *struct {
		Model string `json:"model"`
	} `json:"init"`
	Result *struct {
		ConversationID string             `json:"conversation_id"`
		Status         string             `json:"status"`
		NumTurns       int64              `json:"num_turns"`
		Usage          *streamUsageRecord `json:"usage"`
	} `json:"result"`
}

type streamReadResult struct {
	Snapshots []model.AggregateSnapshot
	Skipped   int
	ScanErr   error
	FormatErr error
}

// candidateDirs are the Antigravity data roots, relative to the user's home,
// probed for usage-bearing files.
var candidateDirs = [][]string{
	{".gemini", "antigravity-cli"},
	{".antigravitycli"},
	{".cache", "antigravity"},
}

// Adapter reads Antigravity CLI telemetry files. Read-only.
type Adapter struct{}

// New returns an Antigravity adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolAgy }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Antigravity" }

// Capabilities declares what this project can say about Antigravity.
//
// Cost is COMPUTED: nothing here calls SetCost. There is NO activity at all —
// this adapter references model.ActivityEvent nowhere, so its surface exposes
// usage and nothing else.
func (Adapter) Capabilities() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolAgy,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(model.ToolAgy),
		Tier:      model.TierLive,
	}
}

// roots returns the Antigravity data directories to scan: an explicit override
// when present, otherwise the canonical home-relative candidates.
func (a Adapter) roots(cfg adapter.DiscoverConfig) []string {
	if cfg.Overrides != nil {
		if v := strings.TrimSpace(cfg.Overrides[model.ToolAgy]); v != "" {
			return []string{v}
		}
	}
	if cfg.Home == "" {
		return nil
	}
	out := make([]string, 0, len(candidateDirs))
	for _, parts := range candidateDirs {
		out = append(out, filepath.Join(append([]string{cfg.Home}, parts...)...))
	}
	return out
}

// Discover scans each Antigravity root for *.json / *.jsonl files. Files are
// not pre-parsed for token usage here. Content-only artifacts remain valid
// empty sources, while captured print-mode streams provide cumulative usage.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source
	for _, root := range a.roots(cfg) {
		if root == "" || !adapter.IsDir(root) {
			continue
		}
		// Aggregate keys embed absolute file paths, so resolve symlinks: a
		// re-pointed root would otherwise mint new identities and re-add full
		// cumulative totals. A genuinely moved root still re-adds once —
		// irreducible without state migration.
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if d.IsDir() {
				return nil
			}
			if !geminishape.HasUsageExt(path) {
				return nil
			}
			if !adapter.WalkEntryIsFile(d, path) {
				return nil
			}
			if _, dup := seen[path]; dup {
				return nil
			}
			seen[path] = struct{}{}
			srcs = append(srcs, adapter.Source{
				Tool:  model.ToolAgy,
				Class: model.Aggregate,
				Path:  path,
				Label: "Antigravity turns: " + path,
				Meta:  map[string]string{"root": root},
			})
			return nil
		})
	}
	return srcs, nil
}

// Collect reads a single file and emits one AggregateSnapshot per (file, id),
// taking the max (final) cumulative snapshot per id. Malformed records are
// skipped; a non-fatal error is returned describing how many were skipped.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental gates the file on size+mtime: unchanged files are not
// opened at all. Any change re-parses the whole file (cumulative records need
// the max-per-id grouping over every record). A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	fi, err := os.Stat(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("agy: stat %s: %w", src.Path, err)
	}
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()
	if cp != nil && cp.Size == size && cp.MTimeNS == mtimeNS {
		return adapter.Observation{}, nil // unchanged: skip, keep stored checkpoint
	}

	now := time.Now().UTC()
	stream, err := isStreamFile(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("agy: inspect %s: %w", src.Path, err)
	}
	if stream {
		return collectStreamFile(src, size, mtimeNS, now)
	}

	res, err := shape.ReadFile(src.Path, now)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("agy: read %s: %w", src.Path, err)
	}

	obs := adapter.Observation{Snapshots: res.Snapshots}
	if res.ScanErr == nil {
		// The read completed (skipped lines are permanently unparseable, not
		// partial), so the checkpoint may advance. A scan abort withholds it:
		// the unread remainder must be re-read next cycle.
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: model.ToolAgy, SourcePath: src.Path, Size: size, MTimeNS: mtimeNS,
		}
	}
	switch {
	case res.ScanErr != nil && res.Skipped > 0:
		// Both happened in this read: report them together rather than dropping
		// the skip count, which the partial-read error alone would hide.
		return obs, fmt.Errorf("agy: partial read of %s (%d unparseable record(s) skipped): %w", src.Path, res.Skipped, res.ScanErr)
	case res.ScanErr != nil:
		return obs, fmt.Errorf("agy: partial read of %s: %w", src.Path, res.ScanErr)
	case res.Skipped > 0:
		return obs, fmt.Errorf("agy: skipped %d unparseable record(s) in %s", res.Skipped, src.Path)
	}
	return obs, nil
}

// isStreamFile recognizes the first nonempty print-mode stream record. A
// scanner failure falls back to the legacy parser, which owns partial-read
// reporting for legacy files and preserves its existing behavior.
func isStreamFile(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxStreamRecordBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var header struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(line, &header) != nil {
			return false, nil
		}
		switch header.Event {
		case "init", "step_update", "result":
			return true, nil
		default:
			return false, nil
		}
	}
	return false, nil
}

func collectStreamFile(src adapter.Source, size, mtimeNS int64, now time.Time) (adapter.Observation, error) {
	res, err := readStreamFile(src.Path, now)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("agy: read %s: %w", src.Path, err)
	}

	obs := adapter.Observation{Snapshots: res.Snapshots}
	if res.ScanErr == nil && res.FormatErr == nil {
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: model.ToolAgy, SourcePath: src.Path, Size: size, MTimeNS: mtimeNS,
		}
	}

	var readErrs []error
	if res.FormatErr != nil {
		readErrs = append(readErrs, fmt.Errorf("agy: incompatible stream %s: %w", src.Path, res.FormatErr))
	}
	if res.ScanErr != nil {
		readErrs = append(readErrs, fmt.Errorf("agy: partial read of %s: %w", src.Path, res.ScanErr))
	}
	if res.Skipped > 0 {
		readErrs = append(readErrs, fmt.Errorf("agy: skipped %d unparseable record(s) in %s", res.Skipped, src.Path))
	}
	return obs, errors.Join(readErrs...)
}

func readStreamFile(path string, now time.Time) (streamReadResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return streamReadResult{}, err
	}
	defer f.Close()

	models := make(map[string]string)
	latest := make(map[string]model.AggregateSnapshot)
	var out streamReadResult
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxStreamRecordBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec streamRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			out.Skipped++
			continue
		}
		switch rec.Event {
		case "init":
			if rec.ConversationID == "" || rec.Init == nil || rec.Init.Model == "" {
				out.FormatErr = errors.Join(out.FormatErr, streamFormatError("init record missing conversation_id or init.model"))
				continue
			}
			models[rec.ConversationID] = rec.Init.Model
		case "step_update":
			// Its usage is the current step only. The result record below is the
			// authoritative cumulative conversation counter.
		case "result":
			if rec.Result == nil || rec.Result.ConversationID == "" || rec.Result.Status == "" || rec.Result.NumTurns <= 0 {
				out.FormatErr = errors.Join(out.FormatErr, streamFormatError("result record missing conversation_id, status, or num_turns"))
				continue
			}
			if rec.Result.Usage == nil {
				if strings.EqualFold(rec.Result.Status, "SUCCESS") {
					out.FormatErr = errors.Join(out.FormatErr, streamFormatError("successful result record missing usage"))
				}
				continue
			}
			modelName := models[rec.Result.ConversationID]
			if modelName == "" {
				out.FormatErr = errors.Join(out.FormatErr, streamFormatError("result record has no matching init.model"))
				continue
			}
			u, err := normalizeStreamUsage(rec.Result.Usage)
			if err != nil {
				out.FormatErr = errors.Join(out.FormatErr, err)
				continue
			}
			raw, _ := json.Marshal(struct {
				Model    string      `json:"model"`
				NumTurns int64       `json:"num_turns"`
				Usage    streamUsage `json:"usage"`
			}{Model: modelName, NumTurns: rec.Result.NumTurns, Usage: u})
			latest[rec.Result.ConversationID] = model.AggregateSnapshot{
				Tool:            model.ToolAgy,
				Key:             rec.Result.ConversationID,
				Model:           modelName,
				Provider:        model.ProviderGoogle,
				SessionID:       rec.Result.ConversationID,
				Project:         metaProject,
				ObservedTime:    now,
				InputTokens:     u.InputTokens - u.CacheReadTokens,
				OutputTokens:    u.OutputTokens,
				CacheReadTokens: u.CacheReadTokens,
				ReasoningTokens: u.ThinkingTokens,
				TotalTokens:     u.TotalTokens,
				SourcePath:      path,
				Raw:             string(raw),
			}
		case "":
			out.FormatErr = errors.Join(out.FormatErr, streamFormatError("record missing event discriminator"))
		default:
			// Additive event kinds are forward-compatible and carry no usage.
		}
	}
	out.ScanErr = scanner.Err()

	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := latest[id]
		if s.InputTokens != 0 || s.OutputTokens != 0 || s.ReasoningTokens != 0 || s.CacheReadTokens != 0 || s.TotalTokens != 0 {
			out.Snapshots = append(out.Snapshots, s)
		}
	}
	return out, nil
}

func normalizeStreamUsage(raw *streamUsageRecord) (streamUsage, error) {
	if raw.InputTokens == nil || raw.OutputTokens == nil || raw.ThinkingTokens == nil || raw.CacheReadTokens == nil || raw.TotalTokens == nil {
		return streamUsage{}, streamFormatError("usage is missing a required token counter")
	}
	u := streamUsage{
		InputTokens:     *raw.InputTokens,
		OutputTokens:    *raw.OutputTokens,
		ThinkingTokens:  *raw.ThinkingTokens,
		CacheReadTokens: *raw.CacheReadTokens,
		TotalTokens:     *raw.TotalTokens,
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.ThinkingTokens < 0 || u.CacheReadTokens < 0 || u.TotalTokens < 0 {
		return streamUsage{}, streamFormatError("usage contains a negative counter")
	}
	if u.TotalTokens != u.InputTokens+u.OutputTokens {
		return streamUsage{}, streamFormatError("usage total_tokens does not equal input_tokens + output_tokens")
	}
	if u.ThinkingTokens > u.OutputTokens {
		return streamUsage{}, streamFormatError("usage thinking_tokens exceeds output_tokens")
	}
	if u.CacheReadTokens > u.InputTokens {
		return streamUsage{}, streamFormatError("usage cache_read_tokens exceeds input_tokens")
	}
	return u, nil
}

func streamFormatError(detail string) error {
	return fmt.Errorf("%s: %w", detail, adapter.ErrSourceFormat)
}
