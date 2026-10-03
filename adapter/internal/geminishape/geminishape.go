// Package geminishape parses the Gemini CLI telemetry file shape shared by the
// gemini and agy adapters, so a parser bug is fixed exactly once. Records for a
// single turn are CUMULATIVE: each new record for a given id re-emits the
// turn's growing running totals, so the last record for an id (equivalently,
// the one with the largest total) holds the final figures. ReadFile therefore
// groups records by id within a file, keeps the max snapshot per id, and emits
// one AggregateSnapshot per (file, id).
//
// Token mapping (per plan section 1):
//
//	Input         = (tokens.input + tokens.tool) - cached overlap   (clamped >= 0)
//	Output        = tokens.output
//	CacheRead     = tokens.cached
//	CacheCreation = 0
//	Reasoning     = tokens.thoughts
//	Total         = max(tokens.total, input+tool+output+thoughts)
//
// The total accounts for every component this shape folds into the event's own
// token fields, so a row can never report a total below the counts stored
// beside it (issue #49). tokens.tool is part of it because it is folded into
// InputTokens; tokens.thoughts is part of it because reasoning is ADDITIVE for
// this shape (model.ReasoningModeFor). Cached is NOT added on top: it is the
// slice of tokens.input that was served from cache, already inside that count,
// and it is reported separately in CacheReadTokens.
//
// tokens.total is the provider's own figure and stays authoritative wherever it
// covers those components; it is only raised when the provider itemised more
// than it totalled (real records do: a turn reporting input+output+thoughts as
// its total leaves its tool tokens out of it).
//
// The adapters keep their own roots/Discover policy and their size+mtime
// checkpoint gates; only the file parsing lives here.
//
// CRITICAL: strictly read-only. Files are opened O_RDONLY and never written,
// locked, or modified.
package geminishape

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/model"
)

// maxLineBytes bounds a single JSONL line so a pathological file cannot
// exhaust memory.
const maxLineBytes = 8 << 20 // 8 MiB

// Shape parameterises the shared parser for one adapter.
type Shape struct {
	Tool     string // tool id stamped on every snapshot (model.ToolGemini, ...)
	Provider string // billing identity stamped on every snapshot (model.ProviderGoogle)
	Project  string // project label (the telemetry records no cwd)
}

// FileResult is the outcome of parsing one telemetry file.
type FileResult struct {
	Snapshots []model.AggregateSnapshot
	// Skipped counts genuinely unparseable lines/entries (permanently bad, a
	// re-read cannot fix them).
	Skipped int
	// ScanErr is non-nil when the JSONL scanner aborted mid-file (e.g. an
	// over-long line): the snapshots above are best-effort and the caller MUST
	// NOT advance its checkpoint — the unread remainder would be skipped until
	// the next size/mtime change.
	ScanErr error
}

// ReadFile parses path and returns one AggregateSnapshot per (file, id),
// keeping the max (final) cumulative snapshot per id. now supplies ObservedTime
// for records without a parseable timestamp. The error return is fatal only
// (the file could not be opened or read at all).
func (s Shape) ReadFile(path string, now time.Time) (FileResult, error) {
	p, err := readRecords(path)
	if err != nil {
		return FileResult{}, err
	}

	// Group by id, keeping the record with the largest total per id.
	best := make(map[string]rawRecord)
	var order []string
	for _, r := range p.recs {
		cur, ok := best[r.ID]
		if !ok {
			order = append(order, r.ID)
		}
		if !ok || r.total() >= cur.total() {
			best[r.ID] = r
		}
	}

	var snaps []model.AggregateSnapshot
	for _, id := range order {
		snap, ok := s.toSnapshot(best[id], path, now)
		if !ok {
			// Not malformed: session-header records, user turns and $set mutation
			// entries (e.g. {"$set":{"lastUpdated":...}}) carry no token usage and
			// are dropped silently. Only genuinely unparseable lines (counted by
			// readRecords) are reported via Skipped.
			continue
		}
		snaps = append(snaps, snap)
	}
	return FileResult{Snapshots: snaps, Skipped: p.skipped, ScanErr: p.scanErr}, nil
}

// HasUsageExt reports whether a path ends in .json or .jsonl — the extensions
// the Discover walks of both adapters accept.
func HasUsageExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".json" || ext == ".jsonl"
}

// tokenBlock is the per-turn token breakdown emitted by Gemini CLI.
type tokenBlock struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
	Tool     int64 `json:"tool"`
	Total    int64 `json:"total"`
}

// rawRecord is a single decoded telemetry record. A $set wrapper (used by some
// Gemini sinks) is unwrapped before decoding into this shape.
//
// The decoded fields double as the ALLOW-LIST persisted in
// AggregateSnapshot.Raw (see auditJSON): the audit payload is re-marshalled
// from them rather than kept as the original bytes, so nothing the record
// carries outside this shape — prompt or response text in particular — can
// reach the ledger (issue #17).
type rawRecord struct {
	ID        string     `json:"id"`
	Model     string     `json:"model"`
	Type      string     `json:"type"`
	SessionID string     `json:"sessionId"`
	Timestamp string     `json:"timestamp"`
	Tokens    tokenBlock `json:"tokens"`
}

// auditJSON renders the record's allow-listed fields as the stored audit
// payload. Best-effort: an un-marshalable record yields an empty raw rather
// than dropping the snapshot.
func (r rawRecord) auditJSON() string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

// derivedTotal sums the components this shape folds into the event's own token
// fields: input and tool (both land in InputTokens) plus output and thoughts
// (reasoning is ADDITIVE here). Cached is not added on top - it is the slice of
// input that was served from cache and is already inside that count.
func (t tokenBlock) derivedTotal() int64 {
	return adapter.NonNeg(t.Input) + adapter.NonNeg(t.Tool) +
		adapter.NonNeg(t.Output) + adapter.NonNeg(t.Thoughts)
}

// authoritativeTotal reconciles the provider-reported tokens.total against the
// components above: the reported figure wins wherever it covers them, and is
// raised to the component sum when it does not, so the stored total never
// contradicts the counts stored beside it.
func (t tokenBlock) authoritativeTotal() int64 {
	derived := t.derivedTotal()
	if reported := adapter.NonNeg(t.Total); reported > derived {
		return reported
	}
	return derived
}

// total returns the record's authoritative total. Used to pick the max
// (final) snapshot per id, so it must follow the same accounting the snapshot
// itself stores.
func (r rawRecord) total() int64 {
	return r.Tokens.authoritativeTotal()
}

// setWrapper captures the optional `{"$set": {...}}` envelope.
type setWrapper struct {
	Set json.RawMessage `json:"$set"`
}

// messagesBlob is the best-effort `messages[]` shape: each message may carry
// its own usage/tokens block. Records here are treated like top-level records.
type messagesBlob struct {
	Messages []rawRecord `json:"messages"`
}

// parsed is the raw outcome of reading one file, before snapshot mapping.
type parsed struct {
	recs    []rawRecord
	skipped int   // unparseable lines/entries dropped
	scanErr error // non-nil: the JSONL scan aborted mid-file
}

// readRecords parses a *.json or *.jsonl file into decoded records. JSONL is
// parsed line-by-line; .json is parsed as a single value that may be an object,
// an array, a {messages:[...]} blob, or a {$set:{...}} wrapper. The error is
// fatal only (the file itself cannot be opened or read).
func readRecords(path string) (parsed, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return parsed{}, err
	}
	defer f.Close()

	var p parsed

	if strings.EqualFold(filepath.Ext(path), ".jsonl") {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			rs, ok := decodeValue([]byte(line))
			if !ok {
				p.skipped++
				continue
			}
			p.recs = append(p.recs, rs...)
		}
		// A scan error (e.g. an over-long line) aborts mid-file: keep what we
		// have, but surface it so the caller withholds its checkpoint.
		p.scanErr = sc.Err()
		return p, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return parsed{}, err
	}
	rs, ok := decodeValue(data)
	if !ok {
		p.skipped++
		return p, nil
	}
	p.recs = append(p.recs, rs...)
	return p, nil
}

// decodeValue decodes a single JSON value (object, array, messages blob, or
// $set wrapper) into zero or more records. Returns ok=false when the bytes are
// not valid JSON at all.
func decodeValue(data []byte) ([]rawRecord, bool) {
	trimmed := trimLeadingSpace(data)
	if len(trimmed) == 0 {
		return nil, false
	}
	switch trimmed[0] {
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			return nil, false
		}
		var out []rawRecord
		for _, e := range arr {
			if rs, ok := decodeValue(e); ok {
				out = append(out, rs...)
			}
		}
		return out, true
	case '{':
		return decodeObject(data), true
	default:
		return nil, false
	}
}

// decodeObject decodes a JSON object that may be a plain record, a $set
// wrapper, or a {messages:[...]} blob. A single object never reports a parse
// failure for the caller's skip accounting (it is best-effort); empty/zero
// records are filtered later in toSnapshot.
func decodeObject(data []byte) []rawRecord {
	// Unwrap a $set envelope first.
	var w setWrapper
	if err := json.Unmarshal(data, &w); err == nil && len(w.Set) > 0 {
		if rs, ok := decodeValue(w.Set); ok {
			return rs
		}
	}

	// Best-effort messages[] blob: collect any messages carrying tokens.
	var mb messagesBlob
	if err := json.Unmarshal(data, &mb); err == nil && len(mb.Messages) > 0 {
		out := append([]rawRecord(nil), mb.Messages...)
		// A messages blob may ALSO carry top-level tokens (a stats summary);
		// include it too so a summary line is not lost.
		var top rawRecord
		if err := json.Unmarshal(data, &top); err == nil && top.Tokens != (tokenBlock{}) {
			out = append(out, top)
		}
		return out
	}

	var r rawRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil
	}
	return []rawRecord{r}
}

// toSnapshot maps a decoded record onto an AggregateSnapshot. Returns ok=false
// for all-zero (no usage) records, which are dropped per the spec.
func (s Shape) toSnapshot(r rawRecord, sourcePath string, now time.Time) (model.AggregateSnapshot, bool) {
	in := adapter.NonNeg(r.Tokens.Input)
	out := adapter.NonNeg(r.Tokens.Output)
	cached := adapter.NonNeg(r.Tokens.Cached)
	thoughts := adapter.NonNeg(r.Tokens.Thoughts)
	toolTok := adapter.NonNeg(r.Tokens.Tool)

	// Input = (input + tool) minus the cached overlap (cached is reported
	// separately and is a subset of the prompt that was served from cache).
	inputAdj := in + toolTok - cached
	if inputAdj < 0 {
		inputAdj = 0
	}

	// Authoritative total: the provider's tokens.total, raised to cover every
	// component folded into the fields above (tool tokens in particular, which
	// reach InputTokens and which the provider leaves out of its own total).
	total := r.Tokens.authoritativeTotal()

	// Drop all-zero records (no usage to report).
	if inputAdj == 0 && out == 0 && cached == 0 && thoughts == 0 && total == 0 {
		return model.AggregateSnapshot{}, false
	}

	id := strings.TrimSpace(r.ID)
	if id == "" {
		id = "turn" // a record with no id still represents one turn in this file
	}

	session := strings.TrimSpace(r.SessionID)
	if session == "" {
		session = adapter.FileStem(sourcePath)
	}

	obs := now
	if ts := parseTime(r.Timestamp); !ts.IsZero() {
		obs = ts
	}

	return model.AggregateSnapshot{
		Tool:                s.Tool,
		Key:                 sourcePath + "|" + id,
		Model:               strings.TrimSpace(r.Model),
		Provider:            s.Provider,
		SessionID:           session,
		Project:             s.Project,
		ObservedTime:        obs,
		InputTokens:         inputAdj,
		OutputTokens:        out,
		CacheCreationTokens: 0,
		CacheReadTokens:     cached,
		ReasoningTokens:     thoughts,
		TotalTokens:         total,
		SourcePath:          sourcePath,
		Raw:                 r.auditJSON(),
	}, true
}

// parseTime parses an RFC3339 stamp and returns it in UTC, or the zero time
// when the stamp is empty or unparseable. One layout covers both the plain and
// the fractional-second spellings: time.Parse accepts a fractional second
// immediately after the seconds field even when the layout does not carry one,
// so a separate time.RFC3339Nano pass would only ever repeat this one.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// trimLeadingSpace drops leading ASCII whitespace without allocating.
func trimLeadingSpace(b []byte) []byte {
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\n', '\r':
			b = b[1:]
		default:
			return b
		}
	}
	return b
}
