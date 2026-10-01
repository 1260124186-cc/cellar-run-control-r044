package workflowcheck

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

type ledgerRefView struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Version int64  `json:"version"`
}

type ledgerEntryView struct {
	Sequence   int64           `json:"sequence"`
	ID         string          `json:"id"`
	Action     string          `json:"action"`
	OccurredAt time.Time       `json:"occurred_at"`
	Refs       []ledgerRefView `json:"refs"`
	Payload    map[string]any  `json:"payload"`
	PrevHash   string          `json:"prev_hash"`
	EntryHash  string          `json:"entry_hash"`
}

type ledgerPageView struct {
	Entries    []ledgerEntryView `json:"entries"`
	NextCursor string            `json:"next_cursor"`
	Limit      int               `json:"limit"`
}

type healthView struct {
	Status        string `json:"status"`
	Revision      int64  `json:"revision"`
	TrajectorySeq int64  `json:"trajectory_seq"`
}

func trajectoryLedger(api *client, dataDir string) error {
	formula, err := ensureApprovedFormula(api, "Trajectory Red")
	if err != nil {
		return err
	}

	vesselKey := "vessel-key-0001"
	vesselResponse, err := api.callWith("POST", "/v1/vessels", map[string]any{
		"code":            "VSL-LED-1",
		"capacity_liters": 300.0,
	}, map[string]string{"Idempotency-Key": vesselKey})
	if err != nil {
		return err
	}
	if err := requireStatus(vesselResponse, 201, "create vessel with key"); err != nil {
		return err
	}
	var vessel vesselView
	if err := vesselResponse.decode(&vessel); err != nil {
		return err
	}

	runResponse, err := api.call("POST", "/v1/runs",
		createRunPayload("RUN-LEDGER-1", formula.ID), nil)
	if err != nil {
		return err
	}
	if err := requireStatus(runResponse, 201, "create run"); err != nil {
		return err
	}
	var run runView
	if err := runResponse.decode(&run); err != nil {
		return err
	}

	if _, err := api.call("POST", "/v1/runs/"+run.ID+"/reserve",
		map[string]any{"vessel_id": vessel.ID}, ptrInt64(1)); err != nil {
		return err
	}
	if _, err := api.call("POST", "/v1/runs/"+run.ID+"/start", nil, ptrInt64(2)); err != nil {
		return err
	}

	base := time.Now().UTC()
	for sequence := 1; sequence <= 3; sequence++ {
		observed, err := api.call("POST", "/v1/runs/"+run.ID+"/observations",
			map[string]any{
				"sequence":      sequence,
				"gravity":       1.050 - float64(sequence)*0.012,
				"temperature_c": 20.0,
				"observed_at":   base.Add(time.Duration(sequence) * 2 * time.Second).Format(time.RFC3339),
				"note":          fmt.Sprintf("reading %d", sequence),
			}, nil)
		if err != nil {
			return err
		}
		if err := requireStatus(observed, 200, "append observation"); err != nil {
			return err
		}
	}

	if _, err := api.call("POST", "/v1/runs/"+run.ID+"/conditioning", nil, ptrInt64(6)); err != nil {
		return err
	}
	completeResponse, err := api.call("POST", "/v1/runs/"+run.ID+"/complete", nil, ptrInt64(7))
	if err != nil {
		return err
	}
	if err := requireStatus(completeResponse, 200, "complete run"); err != nil {
		return err
	}

	cleanResponse, err := api.call("POST", "/v1/vessels/"+vessel.ID+"/finish-cleaning",
		nil, ptrInt64(4))
	if err != nil {
		return err
	}
	if err := requireStatus(cleanResponse, 200, "finish cleaning"); err != nil {
		return err
	}

	health, err := api.call("GET", "/healthz", nil, nil)
	if err != nil {
		return err
	}
	var healthBody healthView
	if err := health.decode(&healthBody); err != nil {
		return err
	}
	// 3 formula actions + vessel + run + reserve + start + 3 observations +
	// conditioning + complete + vessel cleaning started + finish-cleaning.
	if healthBody.TrajectorySeq != 14 {
		return fmt.Errorf("expected trajectory_seq 14, got %d", healthBody.TrajectorySeq)
	}

	full, err := collectLedgerPages(api, "/v1/trajectory", 2)
	if err != nil {
		return err
	}
	if len(full) != 14 {
		return fmt.Errorf("expected 14 ledger rows across pages, got %d", len(full))
	}
	if err := verifyLedgerChain(full); err != nil {
		return err
	}
	expectedActions := []string{
		"formula_created", "formula_steps_set", "formula_approved",
		"vessel_created", "run_created", "vessel_reserved",
		"fermentation_started", "observation_recorded",
		"observation_recorded", "observation_recorded",
		"conditioning_started", "run_completed",
		"vessel_cleaning_started", "vessel_cleaning_finished",
	}
	for index, expected := range expectedActions {
		if full[index].Action != expected {
			return fmt.Errorf("ledger action %d: expected %q, got %q",
				index+1, expected, full[index].Action)
		}
	}

	// Lineage reads: each entity timeline follows stored sequence order.
	runTimeline, err := collectLedgerPages(api, "/v1/runs/"+run.ID+"/trajectory", 4)
	if err != nil {
		return err
	}
	if len(runTimeline) != 10 {
		return fmt.Errorf("run timeline expected 10 rows, got %d", len(runTimeline))
	}
	if runTimeline[0].Action != "run_created" || runTimeline[len(runTimeline)-1].Action != "vessel_cleaning_finished" {
		return fmt.Errorf("run timeline has unexpected endpoints")
	}
	for _, entry := range runTimeline {
		if !entryTouches(entry, "run", run.ID) {
			return fmt.Errorf("run timeline leaked an unrelated row: %s", entry.Action)
		}
	}

	vesselTimeline, err := collectLedgerPages(api, "/v1/vessels/"+vessel.ID+"/trajectory", 50)
	if err != nil {
		return err
	}
	if len(vesselTimeline) != 10 {
		return fmt.Errorf("vessel timeline expected 10 rows, got %d", len(vesselTimeline))
	}
	if vesselTimeline[0].Action != "vessel_created" ||
		vesselTimeline[len(vesselTimeline)-1].Action != "vessel_cleaning_finished" {
		return fmt.Errorf("vessel timeline has unexpected endpoints")
	}

	formulaTimeline, err := collectLedgerPages(api, "/v1/formulas/"+formula.ID+"/trajectory", 50)
	if err != nil {
		return err
	}
	if len(formulaTimeline) != 13 {
		return fmt.Errorf("formula lineage expected 13 rows, got %d", len(formulaTimeline))
	}

	// Single-row reads, including the not-found code.
	singleResponse, err := api.call("GET", "/v1/trajectory/entries/"+full[11].ID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(singleResponse, 200, "read ledger entry"); err != nil {
		return err
	}
	var completedEntry ledgerEntryView
	if err := singleResponse.decode(&completedEntry); err != nil {
		return err
	}
	if completedEntry.Payload["vessel_state"] != "cleaning" {
		return fmt.Errorf("completion entry did not record vessel cleaning")
	}
	if metrics, ok := completedEntry.Payload["metrics"].(map[string]any); !ok ||
		metrics["observation_count"].(float64) != 3 {
		return fmt.Errorf("completion entry lost its metrics payload")
	}
	missing, err := api.call("GET", "/v1/trajectory/entries/ledger_does_not_exist", nil, nil)
	if err != nil {
		return err
	}
	if err := requireCode(missing, 404, string(domain.CodeTrajectoryNotFound),
		"read unknown ledger entry"); err != nil {
		return err
	}

	// Idempotency: same key + same parameters replays without a new row.
	replayResponse, err := api.callWith("POST", "/v1/vessels", map[string]any{
		"code":            "VSL-LED-1",
		"capacity_liters": 300.0,
	}, map[string]string{"Idempotency-Key": vesselKey})
	if err != nil {
		return err
	}
	if err := requireStatus(replayResponse, 201, "replay idempotent create"); err != nil {
		return err
	}
	if replayResponse.headers.Get("Idempotency-Replayed") != "true" {
		return fmt.Errorf("replayed request did not carry the replay header")
	}
	var replayedVessel vesselView
	if err := replayResponse.decode(&replayedVessel); err != nil {
		return err
	}
	if replayedVessel.ID != vessel.ID {
		return fmt.Errorf("idempotent replay returned a different vessel")
	}
	conflictingKey, err := api.callWith("POST", "/v1/vessels", map[string]any{
		"code":            "VSL-LED-1",
		"capacity_liters": 450.0,
	}, map[string]string{"Idempotency-Key": vesselKey})
	if err != nil {
		return err
	}
	if err := requireCode(conflictingKey, 409, string(domain.CodeIdempotencyConflict),
		"reuse key with different parameters"); err != nil {
		return err
	}

	// A failed transition must not leave a ledger row.
	repeatedComplete, err := api.call("POST", "/v1/runs/"+run.ID+"/complete",
		nil, ptrInt64(8))
	if err != nil {
		return err
	}
	if err := requireCode(repeatedComplete, 409, string(domain.CodeRunState),
		"complete an already completed run"); err != nil {
		return err
	}
	afterFailure, err := collectLedgerPages(api, "/v1/trajectory", 200)
	if err != nil {
		return err
	}
	if len(afterFailure) != 14 {
		return fmt.Errorf("failed action appended %d unexpected ledger rows",
			len(afterFailure)-14)
	}

	// NDJSON export preserves the exact same total order.
	exportResponse, err := api.call("GET", "/v1/trajectory/export", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(exportResponse, 200, "export ledger"); err != nil {
		return err
	}
	exported, err := parseNDJSON(exportResponse.body)
	if err != nil {
		return err
	}
	if len(exported) != 14 {
		return fmt.Errorf("export expected 14 rows, got %d", len(exported))
	}
	for index := range full {
		if exported[index].Sequence != full[index].Sequence ||
			exported[index].ID != full[index].ID {
			return fmt.Errorf("export order diverged at row %d", index+1)
		}
	}

	// Time-window cursor: since the first action returns every row, an until
	// before it returns none.
	firstOccurred := full[0].OccurredAt.UTC()
	sincePath := "/v1/trajectory?since=" + firstOccurred.Format(time.RFC3339Nano)
	sincePage, err := collectLedgerPages(api, sincePath, 200)
	if err != nil {
		return err
	}
	if len(sincePage) != 14 {
		return fmt.Errorf("since-window expected all 14 rows, got %d", len(sincePage))
	}
	untilPath := "/v1/trajectory?until=" +
		firstOccurred.Add(-time.Second).Format(time.RFC3339Nano)
	untilPage, err := collectLedgerPages(api, untilPath, 200)
	if err != nil {
		return err
	}
	if len(untilPage) != 0 {
		return fmt.Errorf("until-window before ledger start must be empty")
	}

	// Restart: a freshly opened store must validate the chain and serve the
	// same rows in the same order from the same cursors.
	reopenedStore, err := storage.Open(dataDir)
	if err != nil {
		return fmt.Errorf("reopen ledger store: %w", err)
	}
	reopenedApp := service.New(reopenedStore, clock.System{})
	reopenedServer := httptest.NewServer(httpapi.NewHandler(reopenedApp))
	defer reopenedServer.Close()
	reopenedAPI := newClient(reopenedServer.URL)

	reopenedHealth, err := reopenedAPI.call("GET", "/healthz", nil, nil)
	if err != nil {
		return err
	}
	var reopenedHealthBody healthView
	if err := reopenedHealth.decode(&reopenedHealthBody); err != nil {
		return err
	}
	if reopenedHealthBody.TrajectorySeq != 14 {
		return fmt.Errorf("after restart trajectory_seq = %d, want 14",
			reopenedHealthBody.TrajectorySeq)
	}
	reopenedRows, err := collectLedgerPages(reopenedAPI, "/v1/trajectory", 3)
	if err != nil {
		return err
	}
	if len(reopenedRows) != 14 {
		return fmt.Errorf("after restart expected 14 rows, got %d", len(reopenedRows))
	}
	for index := range full {
		if reopenedRows[index].Sequence != full[index].Sequence ||
			reopenedRows[index].EntryHash != full[index].EntryHash ||
			reopenedRows[index].PrevHash != full[index].PrevHash {
			return fmt.Errorf("ledger order or hashes changed after restart at row %d", index+1)
		}
	}
	return nil
}

func collectLedgerPages(api *client, path string, limit int) ([]ledgerEntryView, error) {
	var collected []ledgerEntryView
	next := path
	seen := map[int64]bool{}
	for {
		requestPath := next
		if !bytes.Contains([]byte(next), []byte("?")) {
			requestPath = fmt.Sprintf("%s?limit=%d", next, limit)
		} else {
			requestPath = fmt.Sprintf("%s&limit=%d", next, limit)
		}
		response, err := api.call("GET", requestPath, nil, nil)
		if err != nil {
			return nil, err
		}
		if err := requireStatus(response, 200, "read ledger page"); err != nil {
			return nil, err
		}
		var page ledgerPageView
		if err := response.decode(&page); err != nil {
			return nil, err
		}
		for _, entry := range page.Entries {
			if seen[entry.Sequence] {
				return nil, fmt.Errorf("ledger pagination repeated sequence %d", entry.Sequence)
			}
			seen[entry.Sequence] = true
			if len(collected) > 0 && entry.Sequence <= collected[len(collected)-1].Sequence {
				return nil, fmt.Errorf("ledger pagination returned rows out of order")
			}
			collected = append(collected, entry)
		}
		if page.NextCursor == "" {
			return collected, nil
		}
		sep := "?"
		if bytes.Contains([]byte(path), []byte("?")) {
			sep = "&"
		}
		base := path
		next = fmt.Sprintf("%s%scursor=%s", base, sep, page.NextCursor)
	}
}

func verifyLedgerChain(entries []ledgerEntryView) error {
	prev := ""
	for index, entry := range entries {
		if entry.Sequence != int64(index+1) {
			return fmt.Errorf("ledger sequence %d is not contiguous", entry.Sequence)
		}
		if entry.PrevHash != prev {
			return fmt.Errorf("ledger entry %d breaks the hash chain", entry.Sequence)
		}
		if entry.EntryHash == "" {
			return fmt.Errorf("ledger entry %d has no hash", entry.Sequence)
		}
		prev = entry.EntryHash
	}
	return nil
}

func entryTouches(entry ledgerEntryView, kind, id string) bool {
	for _, ref := range entry.Refs {
		if ref.Kind == kind && ref.ID == id {
			return true
		}
	}
	return false
}

func parseNDJSON(raw []byte) ([]ledgerEntryView, error) {
	entries := make([]ledgerEntryView, 0)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry ledgerEntryView
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("parse exported ledger row: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
