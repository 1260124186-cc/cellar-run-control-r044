package ledger

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func testTime(minute int) time.Time {
	return time.Date(2026, 10, 1, 9, minute, 0, 0, time.UTC)
}

func sampleEntry(action Action, subject Ref, version int64, occurred time.Time, lineage ...Ref) Entry {
	return Entry{
		Action:        action,
		OccurredAt:    occurred,
		Subject:       subject,
		EntityVersion: version,
		FromState:     "draft",
		ToState:       "approved",
		Lineage:       lineage,
	}
}

func TestAppendAssignsGaplessSequences(t *testing.T) {
	log := New()
	formula := Ref{Type: EntityFormula, ID: "formula_1"}
	for index := 1; index <= 5; index++ {
		entry, err := log.Append(sampleEntry(
			ActionFormulaApproved, formula, int64(index), testTime(index)))
		if err != nil {
			t.Fatalf("append %d: %v", index, err)
		}
		if entry.Sequence != int64(index) {
			t.Fatalf("entry %d got sequence %d", index, entry.Sequence)
		}
	}
	if log.Len() != 5 || log.LastSequence() != 5 {
		t.Fatalf("unexpected log size: len=%d last=%d", log.Len(), log.LastSequence())
	}
	seen := []int64{}
	err := log.Walk(Filter{}, func(entry Entry) error {
		seen = append(seen, entry.Sequence)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(seen) != 5 {
		t.Fatalf("walk visited %d entries", len(seen))
	}
	for index, sequence := range seen {
		if sequence != int64(index+1) {
			t.Fatalf("walk order not gapless: %v", seen)
		}
	}
}

func TestAppendIdempotentForSameEntityVersion(t *testing.T) {
	log := New()
	run := Ref{Type: EntityRun, ID: "run_1"}
	formula := Ref{Type: EntityFormula, ID: "formula_9"}
	candidate := sampleEntry(ActionRunStarted, run, 3, testTime(1), formula)
	candidate.Detail = map[string]any{"gravity": 1.05}

	first, err := log.Append(candidate)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	second, err := log.Append(candidate)
	if err != nil {
		t.Fatalf("idempotent reprocess must succeed: %v", err)
	}
	if first.Sequence != second.Sequence || log.Len() != 1 {
		t.Fatalf("reprocessing created a duplicate row: first=%d second=%d len=%d",
			first.Sequence, second.Sequence, log.Len())
	}
}

func TestAppendConflictsWhenSameVersionDiffers(t *testing.T) {
	log := New()
	run := Ref{Type: EntityRun, ID: "run_1"}
	candidate := sampleEntry(ActionRunCompleted, run, 8, testTime(1))
	if _, err := log.Append(candidate); err != nil {
		t.Fatalf("first append: %v", err)
	}
	candidate.ToState = "aborted"
	_, err := log.Append(candidate)
	if err == nil {
		t.Fatal("expected conflict for the same idempotency key with a new payload")
	}
	var ledgerErr *Error
	if !errors.As(err, &ledgerErr) || ledgerErr.Kind != KindConflict {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if log.Len() != 1 {
		t.Fatalf("conflicting append must not add a row: len=%d", log.Len())
	}
}

func TestDistinctEntitiesCanShareVersionNumbers(t *testing.T) {
	log := New()
	first := Ref{Type: EntityVessel, ID: "vessel_1"}
	second := Ref{Type: EntityVessel, ID: "vessel_2"}
	if _, err := log.Append(sampleEntry(ActionVesselCreated, first, 1, testTime(0))); err != nil {
		t.Fatalf("append first: %v", err)
	}
	if _, err := log.Append(sampleEntry(ActionVesselCreated, second, 1, testTime(1))); err != nil {
		t.Fatalf("different entities may each start at version 1: %v", err)
	}
	if log.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", log.Len())
	}
}

func TestEntryValidation(t *testing.T) {
	valid := sampleEntry(ActionFormulaApproved,
		Ref{Type: EntityFormula, ID: "formula_1"}, 1, testTime(0))

	cases := []struct {
		name    string
		mutate  func(*Entry)
		wantNil bool
	}{
		{"preset sequence rejected", func(e *Entry) { e.Sequence = 9 }, false},
		{"unknown action rejected", func(e *Entry) { e.Action = "mystery" }, false},
		{"action subject mismatch rejected", func(e *Entry) {
			e.Subject = Ref{Type: EntityVessel, ID: "vessel_1"}
		}, false},
		{"zero version rejected", func(e *Entry) { e.EntityVersion = 0 }, false},
		{"zero time rejected", func(e *Entry) { e.OccurredAt = time.Time{} }, false},
		{"empty subject id rejected", func(e *Entry) { e.Subject.ID = "" }, false},
		{"subject repeated in lineage rejected", func(e *Entry) {
			e.Lineage = []Ref{e.Subject}
		}, false},
		{"valid entry accepted", func(e *Entry) {}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := valid
			tc.mutate(&entry)
			_, err := New().Append(entry)
			if tc.wantNil && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if !tc.wantNil && err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestEntityFilterIncludesSubjectAndLineage(t *testing.T) {
	log := New()
	formula := Ref{Type: EntityFormula, ID: "formula_1", Code: "Red"}
	vessel := Ref{Type: EntityVessel, ID: "vessel_1", Code: "VSL-1"}
	run := Ref{Type: EntityRun, ID: "run_1", Code: "RUN-1"}

	if _, err := log.Append(sampleEntry(ActionFormulaApproved, formula, 1, testTime(0))); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(sampleEntry(ActionVesselReserved, vessel, 1, testTime(1), run)); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(sampleEntry(ActionRunReserved, run, 1, testTime(2), formula, vessel)); err != nil {
		t.Fatal(err)
	}

	page := log.Query(Filter{EntityType: EntityRun, EntityID: "run_1"}, 100, 0)
	if len(page.Entries) != 2 {
		t.Fatalf("run bloodline expected 2 entries (lineage + subject), got %d",
			len(page.Entries))
	}
	if page.Entries[0].Action != ActionVesselReserved ||
		page.Entries[1].Action != ActionRunReserved {
		t.Fatalf("run bloodline order wrong: %v", page.Entries)
	}

	page = log.Query(Filter{EntityType: EntityFormula, EntityID: "formula_1"}, 100, 0)
	if len(page.Entries) != 2 {
		t.Fatalf("formula bloodline expected 2 entries, got %d", len(page.Entries))
	}
}

func TestTimeCursorKeepsSequenceOrder(t *testing.T) {
	log := New()
	run := Ref{Type: EntityRun, ID: "run_1"}
	for index := 1; index <= 4; index++ {
		if _, err := log.Append(sampleEntry(
			ActionObservationAdded, run, int64(index), testTime(index))); err != nil {
			t.Fatal(err)
		}
	}
	from := testTime(3)
	page := log.Query(Filter{From: &from}, 100, 0)
	if len(page.Entries) != 2 {
		t.Fatalf("time cursor expected 2 entries, got %d", len(page.Entries))
	}
	if page.Entries[0].EntityVersion != 3 || page.Entries[1].EntityVersion != 4 {
		t.Fatalf("time cursor returned wrong entries")
	}
}

func TestPaginationStableAcrossPages(t *testing.T) {
	log := New()
	run := Ref{Type: EntityRun, ID: "run_1"}
	for index := 1; index <= 7; index++ {
		if _, err := log.Append(sampleEntry(
			ActionObservationAdded, run, int64(index), testTime(index))); err != nil {
			t.Fatal(err)
		}
	}

	var collected []Entry
	cursor := int64(0)
	pages := 0
	for {
		page := log.Query(Filter{}, 3, cursor)
		collected = append(collected, page.Entries...)
		pages++
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Fatal("final page must not carry a cursor")
			}
			break
		}
		next, err := DecodeCursor(page.NextCursor)
		if err != nil {
			t.Fatalf("decode cursor: %v", err)
		}
		cursor = next
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages, got %d", pages)
	}
	if len(collected) != 7 {
		t.Fatalf("pagination lost entries: collected %d", len(collected))
	}
	for index, entry := range collected {
		if entry.Sequence != int64(index+1) {
			t.Fatalf("pagination reordered entries at %d: seq=%d", index, entry.Sequence)
		}
	}

	// Reissuing the same cursor returns the same page (stable reads).
	cursor2, _ := DecodeCursor(EncodeCursor(2))
	again := log.Query(Filter{}, 3, cursor2)
	repeated := log.Query(Filter{}, 3, cursor2)
	if again.Entries[0].Sequence != 3 ||
		repeated.Entries[0].Sequence != again.Entries[0].Sequence {
		t.Fatal("stable cursor did not return the same page start")
	}
}

func TestQueryClampsLimit(t *testing.T) {
	log := New()
	run := Ref{Type: EntityRun, ID: "run_1"}
	for index := 1; index <= 5; index++ {
		if _, err := log.Append(sampleEntry(
			ActionObservationAdded, run, int64(index), testTime(index))); err != nil {
			t.Fatal(err)
		}
	}
	page := log.Query(Filter{}, 0, 0)
	if page.Limit != DefaultPageLimit {
		t.Fatalf("zero limit should default to %d, got %d", DefaultPageLimit, page.Limit)
	}
	page = log.Query(Filter{}, MaxPageLimit+100, 0)
	if page.Limit != MaxPageLimit {
		t.Fatalf("oversized limit should clamp to %d, got %d", MaxPageLimit, page.Limit)
	}
}

func TestCursorEncodingRoundTrip(t *testing.T) {
	for _, sequence := range []int64{0, 1, 42, 1 << 40} {
		encoded := EncodeCursor(sequence)
		decoded, err := DecodeCursor(encoded)
		if err != nil {
			t.Fatalf("decode cursor: %v", err)
		}
		if decoded != sequence {
			t.Fatalf("cursor round trip: %d -> %s -> %d", sequence, encoded, decoded)
		}
	}
	if _, err := DecodeCursor("%%%not-base64"); err == nil {
		t.Fatal("expected malformed cursor rejection")
	}
}

func TestJSONRoundTripPreservesOrderAndIdempotency(t *testing.T) {
	log := New()
	formula := Ref{Type: EntityFormula, ID: "formula_1"}
	run := Ref{Type: EntityRun, ID: "run_1"}
	if _, err := log.Append(sampleEntry(ActionFormulaApproved, formula, 1, testTime(0))); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(sampleEntry(ActionRunCreated, run, 1, testTime(1), formula)); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(log)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Restart: unmarshal into a fresh log (indexes rebuilt from rows).
	var restarted Log
	if err := json.Unmarshal(raw, &restarted); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restarted.Len() != 2 || restarted.LastSequence() != 2 {
		t.Fatalf("restarted log has wrong size: %d", restarted.Len())
	}
	page := restarted.Query(Filter{EntityType: EntityFormula, EntityID: "formula_1"}, 100, 0)
	if len(page.Entries) != 2 {
		t.Fatalf("lineage index not rebuilt after restart: %d entries", len(page.Entries))
	}
	// After restart, reprocessing an already recorded version is still
	// idempotent and cannot create a second row.
	reprocessed := sampleEntry(ActionRunCreated, run, 1, testTime(1), formula)
	entry, err := restarted.Append(reprocessed)
	if err != nil {
		t.Fatalf("post-restart idempotent append: %v", err)
	}
	if entry.Sequence != 2 || restarted.Len() != 2 {
		t.Fatalf("post-restart append duplicated a row: seq=%d len=%d",
			entry.Sequence, restarted.Len())
	}
	// A genuinely new version continues after the persisted tail.
	entry, err = restarted.Append(sampleEntry(ActionRunStarted, run, 2, testTime(2), formula))
	if err != nil {
		t.Fatalf("post-restart append: %v", err)
	}
	if entry.Sequence != 3 {
		t.Fatalf("new append expected sequence 3, got %d", entry.Sequence)
	}
}

func TestBuildRejectsCorruptSequences(t *testing.T) {
	cases := []struct {
		name    string
		entries []Entry
	}{
		{
			name: "starts above one",
			entries: []Entry{
				sampleEntryWithSeq(2, ActionFormulaCreated,
					Ref{Type: EntityFormula, ID: "formula_1"}, 1, testTime(0)),
			},
		},
		{
			name: "gap in sequence",
			entries: []Entry{
				sampleEntryWithSeq(1, ActionFormulaCreated,
					Ref{Type: EntityFormula, ID: "formula_1"}, 1, testTime(0)),
				sampleEntryWithSeq(3, ActionFormulaStepsSet,
					Ref{Type: EntityFormula, ID: "formula_1"}, 2, testTime(1)),
			},
		},
		{
			name: "duplicate idempotency key",
			entries: []Entry{
				sampleEntryWithSeq(1, ActionRunCompleted,
					Ref{Type: EntityRun, ID: "run_1"}, 5, testTime(0)),
				sampleEntryWithSeq(2, ActionRunCompleted,
					Ref{Type: EntityRun, ID: "run_1"}, 5, testTime(1)),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(tc.entries); err == nil {
				t.Fatal("expected corruption error")
			}
		})
	}
}

func TestCloneIsolation(t *testing.T) {
	log := New()
	run := Ref{Type: EntityRun, ID: "run_1"}
	entry, err := log.Append(sampleEntry(ActionObservationAdded, run, 1, testTime(0)))
	if err != nil {
		t.Fatal(err)
	}
	cloned := log.Clone()
	if _, err := cloned.Append(sampleEntry(ActionObservationAdded, run, 2, testTime(1))); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 1 {
		t.Fatal("clone append leaked into the original log")
	}
	entry.Detail = map[string]any{"mutated": true}
	fresh, ok := log.EntryAt(1)
	if !ok || fresh.Detail != nil {
		t.Fatal("returned entry shares mutable detail storage with the log")
	}
}

func sampleEntryWithSeq(sequence int64, action Action, subject Ref, version int64, occurred time.Time) Entry {
	entry := sampleEntry(action, subject, version, occurred)
	entry.Sequence = sequence
	return entry
}
