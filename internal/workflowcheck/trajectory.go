package workflowcheck

import (
	"fmt"
	"net/http/httptest"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ledger"
)

// productionTrajectory drives a batch from formula release through vessel
// cleaning and verifies the append-only production ledger: ordering,
// lineage reads, time cursors, pagination, idempotent reprocessing, full
// export, and identical order after a process restart.
func productionTrajectory(api *client, dataDir string, server *httptest.Server) error {
	// Formula release: created -> steps configured -> approved.
	created, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Trajectory Red"), nil)
	if err != nil {
		return err
	}
	if err := requireStatus(created, 201, "create trajectory formula"); err != nil {
		return err
	}
	var formula formulaView
	if err := created.decode(&formula); err != nil {
		return err
	}
	steps, err := api.call("PUT", "/v1/formulas/"+formula.ID+"/steps",
		formulaStepPayload(), ptrInt64(1))
	if err != nil {
		return err
	}
	if err := requireStatus(steps, 200, "set trajectory steps"); err != nil {
		return err
	}
	approved, err := api.call("POST", "/v1/formulas/"+formula.ID+"/approve",
		nil, ptrInt64(2))
	if err != nil {
		return err
	}
	if err := requireStatus(approved, 200, "approve trajectory formula"); err != nil {
		return err
	}

	vessel, err := createCheckVessel(api, "VSL-TRACE", 300)
	if err != nil {
		return err
	}
	run, err := createTrajectoryRun(api, formula.ID, vessel.ID, "RUN-TRACE")
	if err != nil {
		return err
	}

	// A rejected transition must not add a ledger row.
	tooEarly, err := api.call("POST", "/v1/runs/"+run.ID+"/observations",
		map[string]any{
			"sequence":      1,
			"gravity":       1.05,
			"temperature_c": 20,
			"observed_at":   time.Now().UTC().Add(20 * time.Second),
			"note":          "before start",
		}, nil)
	if err != nil {
		return err
	}
	if err := requireCode(tooEarly, 409, "invalid_run_state",
		"reject observation before fermentation"); err != nil {
		return err
	}

	reserve, err := api.call("POST", "/v1/runs/"+run.ID+"/reserve",
		map[string]any{"vessel_id": vessel.ID}, ptrInt64(1))
	if err != nil {
		return err
	}
	if err := requireStatus(reserve, 200, "reserve trajectory run"); err != nil {
		return err
	}
	var reservedPair pairView
	if err := reserve.decode(&reservedPair); err != nil {
		return err
	}
	start, err := api.call("POST", "/v1/runs/"+run.ID+"/start",
		nil, ptrInt64(reservedPair.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(start, 200, "start trajectory run"); err != nil {
		return err
	}
	var startedPair pairView
	if err := start.decode(&startedPair); err != nil {
		return err
	}

	base := time.Now().UTC().Add(10 * time.Second)
	version := startedPair.Run.Version
	for sequence := 1; sequence <= 3; sequence++ {
		observation, err := api.call("POST", "/v1/runs/"+run.ID+"/observations",
			map[string]any{
				"sequence":      sequence,
				"gravity":       1.060 - float64(sequence)*0.015,
				"temperature_c": 20.0,
				"observed_at":   verifiedAt(base, sequence),
				"note":          fmt.Sprintf("reading %d", sequence),
			}, ptrInt64(version))
		if err != nil {
			return err
		}
		if err := requireStatus(observation, 200, "append trajectory observation"); err != nil {
			return err
		}
		var updated runView
		if err := observation.decode(&updated); err != nil {
			return err
		}
		version = updated.Version
	}

	outOfRange, err := api.call("POST", "/v1/runs/"+run.ID+"/observations",
		map[string]any{
			"sequence":      4,
			"gravity":       1.9,
			"temperature_c": 20.0,
			"observed_at":   verifiedAt(base, 4),
		}, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireCode(outOfRange, 400, "invalid_input",
		"reject out-of-range trajectory observation"); err != nil {
		return err
	}

	conditioning, err := api.call("POST", "/v1/runs/"+run.ID+"/conditioning",
		nil, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(conditioning, 200, "begin trajectory conditioning"); err != nil {
		return err
	}
	var conditioningView runView
	if err := conditioning.decode(&conditioningView); err != nil {
		return err
	}
	complete, err := api.call("POST", "/v1/runs/"+run.ID+"/complete",
		nil, ptrInt64(conditioningView.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(complete, 200, "complete trajectory run"); err != nil {
		return err
	}
	var completedPair pairView
	if err := complete.decode(&completedPair); err != nil {
		return err
	}
	finish, err := api.call("POST", "/v1/vessels/"+vessel.ID+"/finish-cleaning",
		nil, ptrInt64(completedPair.Vessel.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(finish, 200, "finish trajectory cleaning"); err != nil {
		return err
	}
	var finishedVessel vesselView
	if err := finish.decode(&finishedVessel); err != nil {
		return err
	}

	// Re-processing the same completed version is a stable conflict and
	// must not append a duplicate ledger row.
	duplicateFinish, err := api.call("POST", "/v1/vessels/"+vessel.ID+"/finish-cleaning",
		nil, ptrInt64(finishedVessel.Version))
	if err != nil {
		return err
	}
	if err := requireCode(duplicateFinish, 409, "invalid_vessel_state",
		"reject duplicate cleaning completion"); err != nil {
		return err
	}

	if err := verifyTrajectoryShape(api, formula, vessel, run); err != nil {
		return err
	}

	before, _, err := api.trajectoryExport("/v1/trajectory/export")
	if err != nil {
		return err
	}
	if len(before) != 16 {
		return errInvalidWorkflow(fmt.Sprintf(
			"full export expected 16 entries, got %d", len(before)))
	}

	// Restart the process against the same data directory and verify the
	// ledger replays in the same gapless order.
	server.Close()
	restarted, err := startCheckServer(dataDir)
	if err != nil {
		return err
	}
	defer restarted.Close()
	reborn := newClient(restarted.URL)

	page, res, err := reborn.trajectoryGet("/v1/trajectory?limit=500")
	if err != nil {
		return err
	}
	if err := requireStatus(res, 200, "read trajectory after restart"); err != nil {
		return err
	}
	if err := requireExpectedTrajectory(page.Entries, formula, vessel, run); err != nil {
		return err
	}
	after, res, err := reborn.trajectoryExport("/v1/trajectory/export")
	if err != nil {
		return err
	}
	if err := requireStatus(res, 200, "export trajectory after restart"); err != nil {
		return err
	}
	if !sameTrajectoryOrder(before, after) {
		return errInvalidWorkflow("trajectory order changed after restart")
	}
	return nil
}

func verifyTrajectoryShape(
	api *client,
	formula formulaView,
	vessel vesselView,
	run runView,
) error {
	page, res, err := api.trajectoryGet("/v1/trajectory?limit=500")
	if err != nil {
		return err
	}
	if err := requireStatus(res, 200, "read full trajectory"); err != nil {
		return err
	}
	if err := requireExpectedTrajectory(page.Entries, formula, vessel, run); err != nil {
		return err
	}

	// Run bloodline read: every batch action is reachable from the run.
	runPage, res, err := api.trajectoryGet("/v1/trajectory?entity_type=run&entity_id=" +
		run.ID + "&limit=500")
	if err != nil {
		return err
	}
	if err := requireStatus(res, 200, "read run trajectory"); err != nil {
		return err
	}
	expectedRunActions := []ledger.Action{
		ledger.ActionRunCreated,
		ledger.ActionVesselReserved,
		ledger.ActionRunReserved,
		ledger.ActionVesselRunStarted,
		ledger.ActionRunStarted,
		ledger.ActionObservationAdded,
		ledger.ActionObservationAdded,
		ledger.ActionObservationAdded,
		ledger.ActionRunConditioning,
		ledger.ActionRunCompleted,
		ledger.ActionVesselCleaning,
		ledger.ActionVesselCleaned,
	}
	if len(runPage.Entries) != len(expectedRunActions) {
		return errInvalidWorkflow(fmt.Sprintf(
			"run trajectory expected %d entries, got %d",
			len(expectedRunActions), len(runPage.Entries)))
	}
	for index, action := range expectedRunActions {
		if runPage.Entries[index].Action != action {
			return errInvalidWorkflow(fmt.Sprintf(
				"run trajectory position %d expected %s, got %s",
				index+1, action, runPage.Entries[index].Action))
		}
		if !entryTouches(runPage.Entries[index], ledger.EntityRun, run.ID) {
			return errInvalidWorkflow("run trajectory entry does not touch the run")
		}
	}

	// Vessel bloodline read.
	vesselPage, res, err := api.trajectoryGet("/v1/trajectory?entity_type=vessel&entity_id=" +
		vessel.ID + "&limit=500")
	if err != nil {
		return err
	}
	if err := requireStatus(res, 200, "read vessel trajectory"); err != nil {
		return err
	}
	if len(vesselPage.Entries) != 12 {
		return errInvalidWorkflow(fmt.Sprintf(
			"vessel trajectory expected 12 entries, got %d", len(vesselPage.Entries)))
	}

	// Time cursor: entries occurring at or after run start.
	startedAt := runPage.Entries[4].OccurredAt
	fromPage, res, err := api.trajectoryGet("/v1/trajectory?from=" +
		startedAt.UTC().Format(time.RFC3339Nano) + "&limit=500")
	if err != nil {
		return err
	}
	if err := requireStatus(res, 200, "read time-cursor trajectory"); err != nil {
		return err
	}
	if len(fromPage.Entries) != 9 || fromPage.Entries[0].Action != ledger.ActionVesselRunStarted {
		return errInvalidWorkflow("time cursor did not resume at run start")
	}

	// Entity filter and time cursor combine.
	fromRunPage, res, err := api.trajectoryGet("/v1/trajectory?entity_type=run&entity_id=" +
		run.ID + "&from=" + startedAt.UTC().Format(time.RFC3339Nano) + "&limit=500")
	if err != nil {
		return err
	}
	if err := requireStatus(res, 200, "read combined trajectory filter"); err != nil {
		return err
	}
	if len(fromRunPage.Entries) != 9 {
		return errInvalidWorkflow("combined entity/time filter returned the wrong count")
	}

	// Pagination with cursors must reproduce the exact full order.
	if err := verifyTrajectoryPaging(api, page.Entries); err != nil {
		return err
	}

	// Invalid cursors and malformed filters are stable 400s.
	badCursorRes, err := api.call("GET", "/v1/trajectory?cursor=not-a-cursor", nil, nil)
	if err != nil {
		return err
	}
	if err := requireCode(badCursorRes, 400, "invalid_input", "reject bad cursor"); err != nil {
		return err
	}
	missingID, err := api.call("GET", "/v1/trajectory?entity_type=run", nil, nil)
	if err != nil {
		return err
	}
	if err := requireCode(missingID, 400, "invalid_input", "reject entity_type without id"); err != nil {
		return err
	}
	badType, err := api.call("GET", "/v1/trajectory?entity_type=barrel&entity_id=x", nil, nil)
	if err != nil {
		return err
	}
	if err := requireCode(badType, 400, "invalid_input", "reject unknown entity type"); err != nil {
		return err
	}
	badLimit, err := api.call("GET", "/v1/trajectory?limit=9999", nil, nil)
	if err != nil {
		return err
	}
	if err := requireCode(badLimit, 400, "invalid_input", "reject oversized limit"); err != nil {
		return err
	}

	// Full NDJSON export restricted to the run bloodline.
	runExport, res, err := api.trajectoryExport("/v1/trajectory/export?entity_type=run&entity_id=" + run.ID)
	if err != nil {
		return err
	}
	if err := requireStatus(res, 200, "export run trajectory"); err != nil {
		return err
	}
	if len(runExport) != 12 {
		return errInvalidWorkflow(fmt.Sprintf(
			"run export expected 12 entries, got %d", len(runExport)))
	}
	return nil
}

func requireExpectedTrajectory(
	entries []ledger.Entry,
	formula formulaView,
	vessel vesselView,
	run runView,
) error {
	expected := []struct {
		action  ledger.Action
		subject ledger.EntityType
		version int64
	}{
		{ledger.ActionFormulaCreated, ledger.EntityFormula, 1},
		{ledger.ActionFormulaStepsSet, ledger.EntityFormula, 2},
		{ledger.ActionFormulaApproved, ledger.EntityFormula, 3},
		{ledger.ActionVesselCreated, ledger.EntityVessel, 1},
		{ledger.ActionRunCreated, ledger.EntityRun, 1},
		{ledger.ActionVesselReserved, ledger.EntityVessel, 2},
		{ledger.ActionRunReserved, ledger.EntityRun, 2},
		{ledger.ActionVesselRunStarted, ledger.EntityVessel, 3},
		{ledger.ActionRunStarted, ledger.EntityRun, 3},
		{ledger.ActionObservationAdded, ledger.EntityRun, 4},
		{ledger.ActionObservationAdded, ledger.EntityRun, 5},
		{ledger.ActionObservationAdded, ledger.EntityRun, 6},
		{ledger.ActionRunConditioning, ledger.EntityRun, 7},
		{ledger.ActionRunCompleted, ledger.EntityRun, 8},
		{ledger.ActionVesselCleaning, ledger.EntityVessel, 4},
		{ledger.ActionVesselCleaned, ledger.EntityVessel, 5},
	}
	if len(entries) != len(expected) {
		return errInvalidWorkflow(fmt.Sprintf(
			"trajectory expected %d entries, got %d", len(expected), len(entries)))
	}
	var lastOccurred time.Time
	for index, want := range expected {
		entry := entries[index]
		if entry.Sequence != int64(index+1) {
			return errInvalidWorkflow(fmt.Sprintf(
				"trajectory sequence gap at position %d: got %d", index+1, entry.Sequence))
		}
		if entry.Action != want.action {
			return errInvalidWorkflow(fmt.Sprintf(
				"trajectory position %d expected %s, got %s",
				index+1, want.action, entry.Action))
		}
		if entry.Subject.Type != want.subject || entry.EntityVersion != want.version {
			return errInvalidWorkflow(fmt.Sprintf(
				"trajectory %s has wrong subject/version", want.action))
		}
		switch want.subject {
		case ledger.EntityFormula:
			if entry.Subject.ID != formula.ID {
				return errInvalidWorkflow("formula trajectory subject mismatch")
			}
		case ledger.EntityVessel:
			if entry.Subject.ID != vessel.ID {
				return errInvalidWorkflow("vessel trajectory subject mismatch")
			}
		case ledger.EntityRun:
			if entry.Subject.ID != run.ID {
				return errInvalidWorkflow("run trajectory subject mismatch")
			}
		}
		if entry.OccurredAt.Before(lastOccurred) {
			return errInvalidWorkflow("trajectory occurrence times must be ordered")
		}
		lastOccurred = entry.OccurredAt
	}
	return nil
}

func verifyTrajectoryPaging(api *client, full []ledger.Entry) error {
	var collected []ledger.Entry
	cursor := ""
	pageCount := 0
	for {
		path := "/v1/trajectory?limit=5"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page, res, err := api.trajectoryGet(path)
		if err != nil {
			return err
		}
		if err := requireStatus(res, 200, "trajectory page"); err != nil {
			return err
		}
		if len(page.Entries) == 0 {
			return errInvalidWorkflow("trajectory page returned no entries")
		}
		collected = append(collected, page.Entries...)
		pageCount++
		if !page.HasMore {
			if page.NextCursor != "" {
				return errInvalidWorkflow("final trajectory page must not carry a cursor")
			}
			break
		}
		if page.NextCursor == "" {
			return errInvalidWorkflow("paged trajectory must provide a next cursor")
		}
		cursor = page.NextCursor
	}
	if pageCount != 4 {
		return errInvalidWorkflow(fmt.Sprintf(
			"16 entries at limit 5 require 4 pages, got %d", pageCount))
	}
	if !sameTrajectoryOrder(full, collected) {
		return errInvalidWorkflow("paged trajectory order disagrees with the full read")
	}
	return nil
}

func createTrajectoryRun(
	api *client,
	formulaID string,
	vesselID string,
	code string,
) (runView, error) {
	response, err := api.call("POST", "/v1/runs",
		createRunPayload(code, formulaID), nil)
	if err != nil {
		return runView{}, err
	}
	if err := requireStatus(response, 201, "create trajectory run"); err != nil {
		return runView{}, err
	}
	var run runView
	if err := response.decode(&run); err != nil {
		return runView{}, err
	}
	return run, nil
}

func entryTouches(entry ledger.Entry, entityType ledger.EntityType, id string) bool {
	wanted := ledger.Ref{Type: entityType, ID: id}
	if entry.Subject.Equal(wanted) {
		return true
	}
	for _, ref := range entry.Lineage {
		if ref.Equal(wanted) {
			return true
		}
	}
	return false
}

func sameTrajectoryOrder(left, right []ledger.Entry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Sequence != right[index].Sequence ||
			left[index].Action != right[index].Action ||
			left[index].Subject != right[index].Subject {
			return false
		}
	}
	return true
}
