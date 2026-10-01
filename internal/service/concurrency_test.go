package service

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

func newTestApp(t *testing.T) *Service {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return New(store, clock.NewFixed(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)))
}

func TestConcurrentDistinctWritesAppendOrderedRows(t *testing.T) {
	app := newTestApp(t)
	const writers = 12
	var wg sync.WaitGroup
	errors := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _, err := app.CreateVessel(CreateVesselInput{
				Code:      fmt.Sprintf("VSL-C-%03d", index),
				CapacityL: 100,
			})
			errors <- err
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent create failed: %v", err)
		}
	}
	page, err := app.ReadTrajectory(TrajectoryQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != writers {
		t.Fatalf("expected %d rows, got %d", writers, len(page.Entries))
	}
	if err := domain.VerifyTrajectory(page.Entries); err != nil {
		t.Fatalf("concurrent ledger is not a valid chain: %v", err)
	}
	seen := map[int64]bool{}
	for _, entry := range page.Entries {
		if seen[entry.Sequence] {
			t.Fatalf("duplicate sequence %d", entry.Sequence)
		}
		seen[entry.Sequence] = true
	}
}

func TestConcurrentSameIdempotencyKeyAppendsOneRow(t *testing.T) {
	app := newTestApp(t)
	const retries = 16
	var wg sync.WaitGroup
	outcomes := make(chan error, retries)
	created := make(chan string, retries)
	for i := 0; i < retries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vessel, _, err := app.CreateVessel(CreateVesselInput{
				Code: "VSL-CONC-1", CapacityL: 250, IdempotencyKey: "concurrent-key-1",
			})
			if err == nil {
				created <- vessel.ID
			}
			outcomes <- err
		}()
	}
	wg.Wait()
	close(outcomes)
	close(created)
	for err := range outcomes {
		if err != nil {
			t.Fatalf("concurrent idempotent retries must all resolve, got %v", err)
		}
	}
	id := ""
	for vesselID := range created {
		if id == "" {
			id = vesselID
		} else if vesselID != id {
			t.Fatalf("idempotent retries returned different vessels")
		}
	}
	if app.TrajectorySize() != 1 {
		t.Fatalf("expected exactly one ledger row for concurrent retries, got %d",
			app.TrajectorySize())
	}
}

func TestConcurrentReservationsOfOneVesselOnlyOneWins(t *testing.T) {
	app := newTestApp(t)
	formula := mustApproveFormula(t, app)
	vessel, _, err := app.CreateVessel(CreateVesselInput{Code: "VSL-RACE-1", CapacityL: 300})
	if err != nil {
		t.Fatal(err)
	}
	runs := make([]domain.FermentationRun, 0, 4)
	for i := 0; i < 4; i++ {
		run, _, err := app.CreateRun(CreateRunInput{
			Code: fmt.Sprintf("RUN-RACE-%d", i), FormulaID: formula.ID, PlannedVolumeL: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	var wg sync.WaitGroup
	wins, losses := 0, 0
	var mu sync.Mutex
	for _, run := range runs {
		wg.Add(1)
		go func(runID string) {
			defer wg.Done()
			_, _, _, err := app.ReserveRun(runID, ReserveRunInput{VesselID: vessel.ID})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case domain.IsCode(err, domain.CodeVesselUnavailable):
				losses++
			default:
				t.Errorf("unexpected reserve error: %v", err)
			}
		}(run.ID)
	}
	wg.Wait()
	if wins != 1 || losses != len(runs)-1 {
		t.Fatalf("expected 1 win and %d losses, got %d/%d", len(runs)-1, wins, losses)
	}
	// Exactly one reservation row exists, and the chain still verifies.
	page, err := app.ReadTrajectory(TrajectoryQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	reservations := 0
	for _, entry := range page.Entries {
		if entry.Action == domain.ActionVesselReserved {
			reservations++
		}
	}
	if reservations != 1 {
		t.Fatalf("expected one reservation row, got %d", reservations)
	}
	if err := domain.VerifyTrajectory(page.Entries); err != nil {
		t.Fatalf("ledger chain broken after racing reservations: %v", err)
	}
}

func mustApproveFormula(t *testing.T, app *Service) domain.Formula {
	t.Helper()
	formula, _, err := app.CreateFormula(CreateFormulaInput{
		Name: "Race Ale", Style: "Ale",
		TargetOriginalGravity: 1.052, TargetFinalGravity: 1.012,
		MaxFermentationTempC: 22, MinimumDays: 7, MaximumDays: 21,
	})
	if err != nil {
		t.Fatal(err)
	}
	formula, _, err = app.SetFormulaSteps(formula.ID, SetFormulaStepsInput{
		Steps: []domain.FormulaStep{
			{Sequence: 1, Label: "Heat", TemperatureC: 66, HoldMinutes: 60},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	formula, _, err = app.ApproveFormula(formula.ID, VersionInput{})
	if err != nil {
		t.Fatal(err)
	}
	return formula
}
