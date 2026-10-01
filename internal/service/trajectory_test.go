package service

import (
	"testing"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

type seededWorkflow struct {
	app     *Service
	formula domain.Formula
	vessel  domain.Vessel
	run     domain.FermentationRun
	startAt time.Time
	dataDir string
}

func seedCompletedWorkflow(t *testing.T) seededWorkflow {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	app := New(store, clock.NewFixed(start))

	formula, _, err := app.CreateFormula(CreateFormulaInput{
		Name: "Trace Ale", Style: "Ale",
		TargetOriginalGravity: 1.052, TargetFinalGravity: 1.012,
		MaxFermentationTempC: 22, MinimumDays: 7, MaximumDays: 21,
	})
	if err != nil {
		t.Fatalf("create formula: %v", err)
	}
	steps := []domain.FormulaStep{
		{Sequence: 1, Label: "Heat", TemperatureC: 66, HoldMinutes: 60},
		{Sequence: 2, Label: "Cool", TemperatureC: 20, HoldMinutes: 30},
	}
	formula, _, err = app.SetFormulaSteps(formula.ID, SetFormulaStepsInput{Steps: steps})
	if err != nil {
		t.Fatalf("set steps: %v", err)
	}
	formula, _, err = app.ApproveFormula(formula.ID, VersionInput{})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	vessel, _, err := app.CreateVessel(CreateVesselInput{Code: "VSL-T-1", CapacityL: 240})
	if err != nil {
		t.Fatalf("create vessel: %v", err)
	}
	run, _, err := app.CreateRun(CreateRunInput{
		Code: "RUN-T-1", FormulaID: formula.ID, PlannedVolumeL: 180,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, vessel, _, err = app.ReserveRun(run.ID, ReserveRunInput{VesselID: vessel.ID})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	run, vessel, _, err = app.StartRun(run.ID, VersionInput{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for seq := 1; seq <= 3; seq++ {
		run, _, err = app.AppendObservation(run.ID, ObservationInput{
			Sequence:     seq,
			Gravity:      1.050 - float64(seq)*0.012,
			TemperatureC: 20,
			ObservedAt:   start.Add(time.Duration(seq) * 10 * time.Second),
		})
		if err != nil {
			t.Fatalf("observation %d: %v", seq, err)
		}
	}
	run, _, err = app.BeginConditioning(run.ID, VersionInput{})
	if err != nil {
		t.Fatalf("conditioning: %v", err)
	}
	run, vessel, _, err = app.CompleteRun(run.ID, VersionInput{})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	vessel, _, err = app.FinishCleaning(vessel.ID, FinishCleaningInput{})
	if err != nil {
		t.Fatalf("finish cleaning: %v", err)
	}
	return seededWorkflow{app: app, formula: formula, vessel: vessel, run: run, startAt: start, dataDir: dir}
}

func expectedWorkflowActions() []domain.TrajectoryAction {
	return []domain.TrajectoryAction{
		domain.ActionFormulaCreated, domain.ActionFormulaStepsSet, domain.ActionFormulaApproved,
		domain.ActionVesselCreated, domain.ActionRunCreated, domain.ActionVesselReserved,
		domain.ActionFermentationStarted,
		domain.ActionObservationRecorded, domain.ActionObservationRecorded, domain.ActionObservationRecorded,
		domain.ActionConditioningStarted, domain.ActionRunCompleted,
		domain.ActionVesselCleaning, domain.ActionCleaningFinished,
	}
}

func int64Ptr(value int64) *int64 { return &value }

func TestCompletedWorkflowWritesOrderedLedger(t *testing.T) {
	seeded := seedCompletedWorkflow(t)
	page, err := seeded.app.ReadTrajectory(TrajectoryQuery{Limit: 200})
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(page.Entries) != 14 {
		t.Fatalf("expected 14 ledger rows, got %d", len(page.Entries))
	}
	expected := expectedWorkflowActions()
	for index, action := range expected {
		if page.Entries[index].Action != action {
			t.Fatalf("row %d: expected %s got %s", index+1, action, page.Entries[index].Action)
		}
		if page.Entries[index].Sequence != int64(index+1) {
			t.Fatalf("row %d has a non-contiguous sequence", index+1)
		}
	}
	if err := domain.VerifyTrajectory(page.Entries); err != nil {
		t.Fatalf("ledger chain invalid: %v", err)
	}
	// The completion row records final metrics; the following row is the
	// explicit vessel cleaning change.
	if page.Entries[11].Action != domain.ActionRunCompleted {
		t.Fatalf("completion row missing")
	}
	metrics, ok := page.Entries[11].Payload["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("completion row lost metrics")
	}
	if metrics["observation_count"] != 3 {
		t.Fatalf("metrics observation_count mismatch: %v", metrics["observation_count"])
	}
	if page.Entries[12].Action != domain.ActionVesselCleaning {
		t.Fatalf("vessel cleaning change was not recorded as its own row")
	}
}

func TestReadTrajectoryPaginationKeepsStableOrder(t *testing.T) {
	seeded := seedCompletedWorkflow(t)
	var collected []domain.TrajectoryEntry
	cursor := ""
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := seeded.app.ReadTrajectory(TrajectoryQuery{Limit: 4, AfterCursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pageNumber+1, err)
		}
		collected = append(collected, page.Entries...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(collected) != 14 {
		t.Fatalf("pagination collected %d rows, want 14", len(collected))
	}
	expected := expectedWorkflowActions()
	for index, action := range expected {
		if collected[index].Action != action {
			t.Fatalf("paginated order diverged at row %d", index+1)
		}
	}
}

func TestReadTrajectoryFiltersByEntityAndTime(t *testing.T) {
	seeded := seedCompletedWorkflow(t)
	runPage, err := seeded.app.ReadTrajectory(TrajectoryQuery{
		Kind: domain.EntityRun, ID: seeded.run.ID, Limit: 100, MustExist: true,
	})
	if err != nil {
		t.Fatalf("run timeline: %v", err)
	}
	if len(runPage.Entries) != 10 {
		t.Fatalf("run timeline expected 10 rows, got %d", len(runPage.Entries))
	}
	for _, entry := range runPage.Entries {
		if !entry.Touches(domain.EntityRun, seeded.run.ID) {
			t.Fatalf("run timeline leaked %s", entry.Action)
		}
	}

	// With the fixed clock every workflow action is recorded at the same
	// instant, so since/until brackets of that instant return all rows and a
	// bracket ending one nanosecond earlier returns none. Filtering by observed
	// business time rather than request time keeps the window restart-stable.
	instant := seeded.startAt
	allInWindow, err := seeded.app.ReadTrajectory(TrajectoryQuery{
		Since: &instant, Until: &instant, Limit: 100,
	})
	if err != nil {
		t.Fatalf("instant window: %v", err)
	}
	if len(allInWindow.Entries) != 14 {
		t.Fatalf("instant window expected 14 rows, got %d", len(allInWindow.Entries))
	}
	beforeInstant := seeded.startAt.Add(-time.Nanosecond)
	emptyWindow, err := seeded.app.ReadTrajectory(TrajectoryQuery{
		Until: &beforeInstant, Limit: 100,
	})
	if err != nil {
		t.Fatalf("before window: %v", err)
	}
	if len(emptyWindow.Entries) != 0 {
		t.Fatalf("window before the first action must be empty")
	}

	missing := seeded.app.ExportTrajectory(TrajectoryQuery{
		Kind: domain.EntityRun, ID: "run_does_not_exist", MustExist: true,
	}, func(domain.TrajectoryEntry) error { return nil })
	if !domain.IsCode(missing, domain.CodeRunNotFound) {
		t.Fatalf("unknown entity should be not_found, got %v", missing)
	}
}

func TestIdempotentCreateVesselReplaysWithoutSecondRow(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	app := New(store, clock.NewFixed(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)))

	first, replayed, err := app.CreateVessel(CreateVesselInput{
		Code: "VSL-IDEM-1", CapacityL: 100, IdempotencyKey: "vessel-key-0001",
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if replayed {
		t.Fatalf("first create must not be flagged as replay")
	}
	second, replayFlag, err := app.CreateVessel(CreateVesselInput{
		Code: "VSL-IDEM-1", CapacityL: 100, IdempotencyKey: "vessel-key-0001",
	})
	if err != nil {
		t.Fatalf("identical retry should replay: %v", err)
	}
	if !replayFlag || second.ID != first.ID {
		t.Fatalf("retry must return the original vessel with replay flag")
	}
	if app.TrajectorySize() != 1 {
		t.Fatalf("replayed request must not add a ledger row, size=%d", app.TrajectorySize())
	}

	_, _, err = app.CreateVessel(CreateVesselInput{
		Code: "VSL-IDEM-1", CapacityL: 999, IdempotencyKey: "vessel-key-0001",
	})
	if !domain.IsCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("same key with changed parameters must conflict, got %v", err)
	}
	if app.TrajectorySize() != 1 {
		t.Fatalf("conflicting retry must not add a ledger row")
	}
}

func TestFailedBusinessActionLeavesNoLedgerRow(t *testing.T) {
	seeded := seedCompletedWorkflow(t)
	before := seeded.app.TrajectorySize()
	// The run is already completed, so a stale/further completion fails after
	// the expected-version check passes only when a valid version is given.
	_, _, _, err := seeded.app.CompleteRun(seeded.run.ID,
		VersionInput{ExpectedVersion: int64Ptr(seeded.run.Version)})
	if !domain.IsCode(err, domain.CodeRunState) {
		t.Fatalf("expected invalid_run_state, got %v", err)
	}
	if seeded.app.TrajectorySize() != before {
		t.Fatalf("failed action changed the ledger size")
	}

	// An out-of-range observation on a separate fermenting run must likewise
	// add no row.
	other, _, err := seeded.app.CreateRun(CreateRunInput{
		Code: "RUN-T-2", FormulaID: seeded.formula.ID, PlannedVolumeL: 100,
	})
	if err != nil {
		t.Fatalf("create second run: %v", err)
	}
	otherVessel, _, err := seeded.app.CreateVessel(CreateVesselInput{
		Code: "VSL-T-2", CapacityL: 200,
	})
	if err != nil {
		t.Fatalf("create second vessel: %v", err)
	}
	if _, otherVessel, _, err = seeded.app.ReserveRun(other.ID,
		ReserveRunInput{VesselID: otherVessel.ID}); err != nil {
		t.Fatalf("reserve second run: %v", err)
	}
	if _, _, _, err = seeded.app.StartRun(other.ID, VersionInput{}); err != nil {
		t.Fatalf("start second run: %v", err)
	}
	_, _, err = seeded.app.AppendObservation(other.ID, ObservationInput{
		Sequence: 1, Gravity: 9.9, TemperatureC: 20,
		ObservedAt: seeded.startAt,
	})
	if !domain.IsCode(err, domain.CodeInvalidInput) {
		t.Fatalf("expected invalid_input, got %v", err)
	}
	if seeded.app.TrajectorySize() != before+4 {
		t.Fatalf("invalid observation added an unexpected ledger row")
	}
}

func TestExpectedVersionStaleWriteAppendsNothing(t *testing.T) {
	seeded := seedCompletedWorkflow(t)
	before := seeded.app.TrajectorySize()
	_, _, err := seeded.app.BeginConditioning(seeded.run.ID,
		VersionInput{ExpectedVersion: int64Ptr(1)})
	if !domain.IsCode(err, domain.CodeRevisionConflict) {
		t.Fatalf("expected revision_conflict, got %v", err)
	}
	if seeded.app.TrajectorySize() != before {
		t.Fatalf("stale write must not touch the ledger")
	}
}

func TestTrajectoryCursorRoundTrip(t *testing.T) {
	cursor := EncodeTrajectoryCursor(42)
	decoded, err := DecodeTrajectoryCursor(cursor)
	if err != nil || decoded != 42 {
		t.Fatalf("cursor round trip failed: %d %v", decoded, err)
	}
	if zero, err := DecodeTrajectoryCursor(""); err != nil || zero != 0 {
		t.Fatalf("empty cursor must mean position 0")
	}
	if _, err := DecodeTrajectoryCursor("not-a-cursor"); !domain.IsCode(err, domain.CodeInvalidInput) {
		t.Fatalf("garbage cursor must be invalid_input, got %v", err)
	}
}

func TestRestartServesSameOrderAndCursors(t *testing.T) {
	seeded := seedCompletedWorkflow(t)
	before, err := seeded.app.ReadTrajectory(TrajectoryQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Entries) != 3 || before.NextCursor == "" {
		t.Fatalf("expected a full first page with a next cursor")
	}

	reopenedStore, err := storage.Open(seeded.dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened := New(reopenedStore, clock.NewFixed(seeded.startAt))
	after, err := reopened.ReadTrajectory(TrajectoryQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if after.NextCursor != before.NextCursor {
		t.Fatalf("cursor changed across restart")
	}
	for index := range before.Entries {
		if after.Entries[index].EntryHash != before.Entries[index].EntryHash ||
			after.Entries[index].Sequence != before.Entries[index].Sequence {
			t.Fatalf("page contents changed across restart at row %d", index+1)
		}
	}
}

func TestAbortBeforeFermentationReleasesAndRecordsVessel(t *testing.T) {
	app := newTestApp(t)
	formula := mustApproveFormula(t, app)
	vessel, _, err := app.CreateVessel(CreateVesselInput{Code: "VSL-ABORT-1", CapacityL: 300})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := app.CreateRun(CreateRunInput{
		Code: "RUN-ABORT-1", FormulaID: formula.ID, PlannedVolumeL: 150,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, vessel, _, err = app.ReserveRun(run.ID, ReserveRunInput{VesselID: vessel.ID}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	before := app.TrajectorySize()
	run, vessel, _, err = app.AbortRun(run.ID, VersionInput{})
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if run.State != domain.RunAborted {
		t.Fatalf("expected aborted run, got %s", run.State)
	}
	if vessel.State != domain.VesselAvailable || vessel.ActiveRunID != nil {
		t.Fatalf("vessel must be released and unlinked, got %s", vessel.State)
	}
	if app.TrajectorySize() != before+2 {
		t.Fatalf("abort and vessel release must append two rows")
	}
	page, err := app.ReadTrajectory(TrajectoryQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	lastTwo := page.Entries[len(page.Entries)-2:]
	if lastTwo[0].Action != domain.ActionRunAborted ||
		lastTwo[1].Action != domain.ActionVesselReleased {
		t.Fatalf("abort rows mismatch: %s then %s", lastTwo[0].Action, lastTwo[1].Action)
	}
	if err := domain.VerifyTrajectory(page.Entries); err != nil {
		t.Fatalf("ledger invalid after release: %v", err)
	}
}
