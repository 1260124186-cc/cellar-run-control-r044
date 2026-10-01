package storage

import (
	"testing"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ledger"
)

func sampleTrajectoryEntry(version int64, occurred time.Time) ledger.Entry {
	return ledger.Entry{
		Action:        ledger.ActionFormulaApproved,
		OccurredAt:    occurred,
		Subject:       ledger.Ref{Type: ledger.EntityFormula, ID: "formula_one"},
		EntityVersion: version,
		FromState:     string(domain.FormulaDraft),
		ToState:       string(domain.FormulaApproved),
	}
}

// TestUpdateRollsBackBusinessStateWhenLedgerAppendFails proves the atomic
// guarantee: a business mutation followed by a failed trajectory append
// commits neither side.
func TestUpdateRollsBackBusinessStateWhenLedgerAppendFails(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	err = store.Update(nil, func(snapshot *domain.Snapshot) error {
		// A business write that would be visible on its own...
		snapshot.Formulas["formula_one"] = domain.Formula{
			ID:      "formula_one",
			Name:    "Broken Red",
			State:   domain.FormulaDraft,
			Version: 1,
			Steps:   []domain.FormulaStep{},
		}
		// ...followed by an invalid trajectory append. A preset sequence
		// is rejected by the ledger kernel.
		bad := sampleTrajectoryEntry(1, time.Now().UTC())
		bad.Sequence = 99
		if _, err := snapshot.Trajectory.Append(bad); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected the update to fail when the ledger append fails")
	}

	if got := store.Revision(); got != 0 {
		t.Fatalf("business revision advanced despite ledger failure: %d", got)
	}
	err = store.View(func(snapshot domain.Snapshot) error {
		if len(snapshot.Formulas) != 0 {
			t.Fatal("business state was published despite ledger failure")
		}
		if snapshot.Trajectory.Len() != 0 {
			t.Fatal("a ledger row survived the failed transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}

// TestTrajectoryPersistsAndResumesAcrossRestart proves gapless sequence
// continuation and stable ordering after reopening the data directory.
func TestTrajectoryPersistsAndResumesAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	for version := int64(1); version <= 3; version++ {
		entry := sampleTrajectoryEntry(version, base.Add(time.Duration(version)*time.Minute))
		err := store.Update(nil, func(snapshot *domain.Snapshot) error {
			_, err := snapshot.Trajectory.Append(entry)
			return err
		})
		if err != nil {
			t.Fatalf("append %d: %v", version, err)
		}
	}

	// Simulate a process restart against the same state.json.
	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Revision() != 3 {
		t.Fatalf("revision after restart = %d, want 3", reopened.Revision())
	}
	err = reopened.View(func(snapshot domain.Snapshot) error {
		if snapshot.Trajectory.Len() != 3 || snapshot.Trajectory.LastSequence() != 3 {
			t.Fatalf("ledger did not replay after restart: len=%d last=%d",
				snapshot.Trajectory.Len(), snapshot.Trajectory.LastSequence())
		}
		for sequence := int64(1); sequence <= 3; sequence++ {
			entry, ok := snapshot.Trajectory.EntryAt(sequence)
			if !ok || entry.Sequence != sequence {
				t.Fatalf("entry %d missing or reordered after restart", sequence)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	// A new append continues immediately after the persisted tail.
	err = reopened.Update(nil, func(snapshot *domain.Snapshot) error {
		entry, err := snapshot.Trajectory.Append(
			sampleTrajectoryEntry(4, base.Add(4*time.Minute)))
		if err != nil {
			return err
		}
		if entry.Sequence != 4 {
			t.Fatalf("post-restart append got sequence %d, want 4", entry.Sequence)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("append after restart: %v", err)
	}
}

// TestIdempotentReprocessingWithinTransaction proves re-appending the same
// entity version returns the existing row instead of adding a duplicate.
func TestIdempotentReprocessingWithinTransaction(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().UTC()
	err = store.Update(nil, func(snapshot *domain.Snapshot) error {
		entry := sampleTrajectoryEntry(7, now)
		first, err := snapshot.Trajectory.Append(entry)
		if err != nil {
			return err
		}
		second, err := snapshot.Trajectory.Append(entry)
		if err != nil {
			return err
		}
		if first.Sequence != second.Sequence {
			t.Fatal("reprocessing produced two different rows")
		}
		if snapshot.Trajectory.Len() != 1 {
			t.Fatal("reprocessing created a duplicate ledger row")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
}
