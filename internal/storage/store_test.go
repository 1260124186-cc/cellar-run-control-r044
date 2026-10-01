package storage

import (
	"testing"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func TestUpdateRollsBackWhenMutationFails(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	err := store.Update(nil, func(snapshot *domain.Snapshot) error {
		vessel := domain.Vessel{
			ID: "vessel_1", Code: "VSL-1", CapacityL: 100,
			State: domain.VesselAvailable, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		snapshot.Vessels["vessel_1"] = vessel
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionVesselCreated,
			OccurredAt: now,
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityVessel, "vessel_1", 1)},
		}
		if _, err := domain.AppendTrajectory(snapshot, draft, now, "k1", "fp1"); err != nil {
			return err
		}
		// A later validation failure must discard the vessel, the index, and
		// the already-sealed ledger row as one atomic unit.
		return domain.NewError(domain.CodeRunState, "synthetic late failure")
	})
	if !domain.IsCode(err, domain.CodeRunState) {
		t.Fatalf("expected the mutator error, got %v", err)
	}
	if got := store.Revision(); got != 0 {
		t.Fatalf("rolled back revision must stay 0, got %d", got)
	}
	if got := store.TrajectorySize(); got != 0 {
		t.Fatalf("rolled back transaction must leave no ledger rows, got %d", got)
	}
	_ = store.View(func(snapshot domain.Snapshot) error {
		if len(snapshot.Vessels) != 0 {
			t.Fatalf("rolled back vessel must not be visible")
		}
		if len(snapshot.IdempotencyByKey) != 0 {
			t.Fatalf("rolled back idempotency claim must not be visible")
		}
		return nil
	})
}

func TestPersistenceFailurePublishesNothing(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Removing the backing directory makes the atomic write fail after the
	// in-memory candidate has been mutated.
	if err := removeDirectory(dir); err != nil {
		t.Skipf("cannot remove data directory: %v", err)
	}
	err = store.Execute(func(snapshot *domain.Snapshot) error {
		snapshot.Vessels["vessel_1"] = domain.Vessel{ID: "vessel_1", Code: "VSL-1"}
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionVesselCreated,
			OccurredAt: time.Now().UTC(),
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityVessel, "vessel_1", 1)},
		}
		_, err := domain.AppendTrajectory(snapshot, draft, time.Now().UTC(), "", "")
		return err
	})
	if !domain.IsCode(err, domain.CodeStorage) {
		t.Fatalf("expected storage_error, got %v", err)
	}
	if store.Revision() != 0 {
		t.Fatalf("failed persistence must not advance the revision")
	}
	if store.TrajectorySize() != 0 {
		t.Fatalf("failed persistence must not publish a ledger row")
	}
	_ = store.View(func(snapshot domain.Snapshot) error {
		if len(snapshot.Vessels) != 0 {
			t.Fatalf("failed persistence must not publish business state")
		}
		return nil
	})
}

func TestReopenReconstructsLedgerAndIndexes(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	err = store.Execute(func(snapshot *domain.Snapshot) error {
		for index := int64(1); index <= 3; index++ {
			draft := domain.TrajectoryDraft{
				Action:     domain.ActionObservationRecorded,
				OccurredAt: now.Add(time.Duration(index) * time.Minute),
				Refs: []domain.TrajectoryRef{
					domain.NewSubjectRef(domain.EntityRun, "run_1", index),
					domain.NewTrajectoryRef(domain.EntityVessel, "vessel_1"),
				},
			}
			if _, err := domain.AppendTrajectory(snapshot, draft, now, "", ""); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if reopened.TrajectorySize() != 3 {
		t.Fatalf("expected 3 rows after reopen, got %d", reopened.TrajectorySize())
	}
	var hashes []string
	_ = reopened.View(func(snapshot domain.Snapshot) error {
		for _, entry := range snapshot.Trajectory {
			hashes = append(hashes, entry.EntryHash)
			key := (domain.VersionIndexKey{
				Kind: domain.EntityRun, ID: "run_1", Version: entry.Sequence,
			}).String()
			if snapshot.VersionIndex[key] != entry.Sequence {
				t.Fatalf("version index missing %s", key)
			}
		}
		// Entity filtering follows stored sequence order after restart.
		var touchingVessel int
		for _, entry := range snapshot.Trajectory {
			if entry.Touches(domain.EntityVessel, "vessel_1") {
				touchingVessel++
			}
		}
		if touchingVessel != 3 {
			t.Fatalf("expected 3 vessel-linked rows, got %d", touchingVessel)
		}
		return nil
	})
	if len(hashes) != 3 {
		t.Fatalf("expected to read 3 hashes, got %d", len(hashes))
	}
}

func TestOpenRejectsTamperedLedger(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	err = store.Execute(func(snapshot *domain.Snapshot) error {
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionRunCreated,
			OccurredAt: time.Now().UTC(),
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityRun, "run_1", 1)},
		}
		_, err := domain.AppendTrajectory(snapshot, draft, time.Now().UTC(), "", "")
		return err
	})
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	if err := tamperSnapshotFile(statePath(dir)); err != nil {
		t.Fatalf("tamper state file: %v", err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatalf("opening a tampered ledger must fail")
	}
}
