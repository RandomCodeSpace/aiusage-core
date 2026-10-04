package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLedgerProcessCrashRecovery(t *testing.T) {
	const childPath = "AIUSAGE_TEST_CRASH_DB"
	ctx := context.Background()
	if path := os.Getenv(childPath); path != "" {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		batch := claudeBatch(15)
		if _, err := st.ApplyBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		batch.Events[0].DedupKey = "uncommitted"
		if _, skip, err := insertEventsTx(ctx, tx, batch.Events); err != nil || skip != nil {
			t.Fatalf("insert: %v %v", skip, err)
		}
		batch.Checkpoint.Offset = 60
		if err := upsertCheckpoint(ctx, tx, *batch.Checkpoint); err != nil {
			t.Fatal(err)
		}
		os.Exit(23) // No deferred cleanup, rollback, Close, or WAL checkpoint.
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	child := exec.Command(os.Args[0], "-test.run=^TestLedgerProcessCrashRecovery$")
	child.Env = append(os.Environ(), childPath+"="+path)
	output, err := child.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
		t.Fatalf("child did not reach crash point: %v\n%s", err, output)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertClaudeAccounting(t, st, 15, 150)
	cp, err := st.Checkpoint(ctx, "claude-code", "/claude")
	if err != nil || cp == nil || cp.Offset != 15 {
		t.Fatalf("recovered checkpoint=%+v %v", cp, err)
	}
	verification, err := Verify(ctx, path)
	if err != nil || verification.State != VerificationOK {
		t.Fatalf("recovery verification=%+v %v", verification, err)
	}
}
