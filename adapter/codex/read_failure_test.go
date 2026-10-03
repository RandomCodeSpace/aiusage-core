package codex

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/store"
)

// faultReader returns a complete prefix, then a read error or cancellation at EOF.
type faultReader struct {
	*strings.Reader
	left   int
	fault  error
	cancel context.CancelFunc
}

func (r *faultReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		if r.cancel != nil {
			r.cancel()
			return 0, io.EOF
		}
		return 0, r.fault
	}
	if len(p) > r.left {
		p = p[:r.left]
	}
	n, err := r.Reader.Read(p)
	r.left -= n
	return n, err
}

func TestReadAbortWithholdsCheckpointAndReplays(t *testing.T) {
	prefix := "{\"type\":\"turn_context\",\"payload\":{\"model\":\"gpt-5-codex\"}}\n{\"type\":\"event_msg\",\"timestamp\":\"2026-05-29T10:00:00Z\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}}\n"
	body := prefix + "{\"type\":\"event_msg\",\"timestamp\":\"2026-05-29T10:00:01Z\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":20,\"output_tokens\":3,\"total_tokens\":23}}}}\n"
	for _, a := range []Adapter{{}} {
		for _, failure := range []string{"read", "cancel-at-eof", "already-canceled"} {
			t.Run(a.ID()+"/"+failure, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "source.jsonl")
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				fi, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				src := adapter.Source{Tool: a.ID(), Path: path, Meta: map[string]string{"session": "s", "agent": "main"}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				fault := errors.New("injected source read failure")
				reader := &faultReader{Reader: strings.NewReader(body), left: len(prefix), fault: fault}
				expected, wantEvents := fault, 1
				if failure == "cancel-at-eof" {
					reader.cancel, expected = cancel, context.Canceled
				}
				if failure == "already-canceled" {
					cancel()
					expected, wantEvents = context.Canceled, 0
				}
				partial, err := a.collectReader(ctx, src, nil, reader, fi)
				if !errors.Is(err, expected) || partial.Checkpoint != nil || len(partial.Events) != wantEvents {
					t.Fatalf("aborted observation = %+v, %v; want %d events, held checkpoint, %v", partial, err, wantEvents, expected)
				}
				if failure == "read" && !strings.Contains(err.Error(), path) {
					t.Fatalf("read error lacks path: %v", err)
				}
				// Apply the valid prefix, as the collector does even when collection errors.
				ledger, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer ledger.Close()
				got, err := ledger.ApplyObservation(context.Background(), partial.Events, partial.Activity, partial.Checkpoint)
				if err != nil || got.Events != wantEvents {
					t.Fatalf("prefix insert = %+v, %v", got, err)
				}
				// Same bytes, size and mtime. No append is needed to recover the unread tail.
				retry, err := a.CollectIncremental(context.Background(), src, nil)
				if err != nil || len(retry.Events) != 2 || retry.Checkpoint == nil || retry.Checkpoint.Offset != int64(len(body)) {
					t.Fatalf("unchanged-file replay = %+v, %v", retry, err)
				}
				got, err = ledger.ApplyObservation(context.Background(), retry.Events, retry.Activity, retry.Checkpoint)
				if err != nil || got.Events != 2-wantEvents {
					t.Fatalf("replay duplicated/lost prefix: %+v, %v", got, err)
				}
				idle, err := a.CollectIncremental(context.Background(), src, retry.Checkpoint)
				if err != nil || idle.Checkpoint != nil || len(idle.Events) != 0 {
					t.Fatalf("idle replay = %+v, %v", idle, err)
				}
			})
		}
	}
}
