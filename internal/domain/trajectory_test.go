package domain

import (
	"strings"
	"testing"
	"time"
)

func testNow() time.Time {
	return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
}

func testDraft(action TrajectoryAction, subject TrajectoryRef) TrajectoryDraft {
	return TrajectoryDraft{
		Action:     action,
		OccurredAt: testNow(),
		Refs:       []TrajectoryRef{subject},
		Payload: map[string]any{
			"gravity": 1.04,
			"nested":  map[string]any{"b": 2, "a": 1},
		},
	}
}

func fixedID() (string, error) {
	idCounter++
	return "ledger_" + string(rune('a'-1+idCounter)) + "0001", nil
}

var idCounter int

func sealTestEntry(t *testing.T, sequence int64, prev string, action TrajectoryAction) TrajectoryEntry {
	t.Helper()
	entry, err := SealTrajectory(
		testDraft(action, NewSubjectRef(EntityRun, "run_1", sequence)),
		sequence, testNow(), prev, fixedID)
	if err != nil {
		t.Fatalf("seal entry %d: %v", sequence, err)
	}
	return entry
}

func TestSealTrajectoryBuildsVerifiableChain(t *testing.T) {
	idCounter = 0
	entries := []TrajectoryEntry{}
	prev := ""
	for sequence := int64(1); sequence <= 4; sequence++ {
		entry := sealTestEntry(t, sequence, prev, ActionObservationRecorded)
		entries = append(entries, entry)
		prev = entry.EntryHash
	}
	if err := VerifyTrajectory(entries); err != nil {
		t.Fatalf("fresh chain should verify: %v", err)
	}
	if entries[0].PrevHash != "" {
		t.Fatalf("first entry must anchor to an empty previous hash")
	}
	for index := 1; index < len(entries); index++ {
		if entries[index].PrevHash != entries[index-1].EntryHash {
			t.Fatalf("entry %d does not sign the previous entry", index+1)
		}
		if entries[index].EntryHash == entries[index-1].EntryHash {
			t.Fatalf("consecutive hashes must differ")
		}
	}
}

func TestSealTrajectoryRejectsInvalidShapes(t *testing.T) {
	cases := map[string]TrajectoryDraft{
		"unknown action": {
			Action:     "mystery_action",
			OccurredAt: testNow(),
			Refs:       domainSubject(),
		},
		"missing subject": {
			Action:     ActionRunCreated,
			OccurredAt: testNow(),
			Refs:       []TrajectoryRef{NewTrajectoryRef(EntityFormula, "formula_1")},
		},
		"two subjects": {
			Action:     ActionRunCreated,
			OccurredAt: testNow(),
			Refs: []TrajectoryRef{
				NewSubjectRef(EntityRun, "run_1", 1),
				NewSubjectRef(EntityVessel, "vessel_1", 1),
			},
		},
		"subject without version": {
			Action:     ActionRunCreated,
			OccurredAt: testNow(),
			Refs:       []TrajectoryRef{{Kind: EntityRun, ID: "run_1", Role: "subject"}},
		},
		"unknown entity kind": {
			Action:     ActionRunCreated,
			OccurredAt: testNow(),
			Refs:       []TrajectoryRef{{Kind: "tank", ID: "x", Role: "subject", Version: 1}},
		},
		"missing occurrence time": {
			Action: ActionRunCreated,
			Refs:   domainSubject(),
		},
	}
	for name, draft := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := SealTrajectory(draft, 1, testNow(), "", fixedID); err == nil {
				t.Fatalf("expected rejection")
			}
		})
	}
}

func domainSubject() []TrajectoryRef {
	return []TrajectoryRef{NewSubjectRef(EntityRun, "run_1", 1)}
}

func TestVerifyTrajectoryDetectsTampering(t *testing.T) {
	idCounter = 0
	entries := []TrajectoryEntry{}
	prev := ""
	for sequence := int64(1); sequence <= 3; sequence++ {
		entry := sealTestEntry(t, sequence, prev, ActionObservationRecorded)
		entries = append(entries, entry)
		prev = entry.EntryHash
	}

	tampered := append([]TrajectoryEntry(nil), entries...)
	tampered[1].Payload = map[string]any{"gravity": 9.99}
	if err := VerifyTrajectory(tampered); err == nil ||
		!strings.Contains(err.Error(), "hash does not match") {
		t.Fatalf("payload tampering must break the hash, got %v", err)
	}

	deleted := append([]TrajectoryEntry(nil), entries[0], entries[2])
	if err := VerifyTrajectory(deleted); err == nil {
		t.Fatalf("deleting a row must break contiguity")
	}

	reordered := []TrajectoryEntry{entries[0], entries[2], entries[1]}
	if err := VerifyTrajectory(reordered); err == nil {
		t.Fatalf("reordering rows must break the chain")
	}

	gap := append([]TrajectoryEntry(nil), entries...)
	gap[1].Sequence = 9
	if err := VerifyTrajectory(gap); err == nil {
		t.Fatalf("a sequence gap must be rejected")
	}
}

func TestCanonicalDigestIsMapOrderIndependent(t *testing.T) {
	entry := sealTestEntry(t, 1, "", ActionRunCreated)
	first := entry.EntryHash
	entry.Payload = map[string]any{"z": 1, "a": map[string]any{"y": 2, "b": 3}}
	digestA, err := digestEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.Payload = map[string]any{"a": map[string]any{"b": 3, "y": 2}, "z": 1}
	digestB, err := digestEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatalf("canonical digest must not depend on map key order")
	}
	if digestA == first {
		t.Fatalf("different content must hash differently")
	}
}

func TestAppendTrajectoryRejectsDuplicateSubjectVersion(t *testing.T) {
	snapshot := EmptySnapshot()
	draft := testDraft(ActionRunCreated, NewSubjectRef(EntityRun, "run_1", 1))
	if _, err := AppendTrajectory(&snapshot, draft, testNow(), "", ""); err != nil {
		t.Fatalf("first append: %v", err)
	}
	duplicate := testDraft(ActionRunCreated, NewSubjectRef(EntityRun, "run_1", 1))
	_, err := AppendTrajectory(&snapshot, duplicate, testNow(), "", "")
	if !IsCode(err, CodeTrajectoryConflict) {
		t.Fatalf("duplicate aggregate version must conflict, got %v", err)
	}
	if len(snapshot.Trajectory) != 1 {
		t.Fatalf("rejected append must leave no row, got %d", len(snapshot.Trajectory))
	}

	// A later version of the same aggregate is a distinct row.
	next := testDraft(ActionFermentationStarted, NewSubjectRef(EntityRun, "run_1", 2))
	if _, err := AppendTrajectory(&snapshot, next, testNow(), "", ""); err != nil {
		t.Fatalf("next version should append: %v", err)
	}
	if len(snapshot.Trajectory) != 2 {
		t.Fatalf("expected two rows, got %d", len(snapshot.Trajectory))
	}
}

func TestAppendTrajectoryClaimsIdempotencyKeyOnce(t *testing.T) {
	snapshot := EmptySnapshot()
	draft := testDraft(ActionVesselCreated, NewSubjectRef(EntityVessel, "vessel_1", 1))
	if _, err := AppendTrajectory(&snapshot, draft, testNow(), "key-aaaa", "fp-1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	again := testDraft(ActionVesselCreated, NewSubjectRef(EntityVessel, "vessel_2", 1))
	if _, err := AppendTrajectory(&snapshot, again, testNow(), "key-aaaa", "fp-1"); !IsCode(err, CodeIdempotencyConflict) {
		t.Fatalf("reusing a claimed key must conflict, got %v", err)
	}
	if len(snapshot.Trajectory) != 1 {
		t.Fatalf("claimed key must not append a second row")
	}
	record := snapshot.IdempotencyByKey["key-aaaa"]
	if record.EntrySequence != 1 || record.Fingerprint != "fp-1" {
		t.Fatalf("idempotency record points at the wrong entry: %+v", record)
	}
}

func TestRebuildTrajectoryIndexes(t *testing.T) {
	snapshot := EmptySnapshot()
	if _, err := AppendTrajectory(&snapshot,
		testDraft(ActionRunCreated, NewSubjectRef(EntityRun, "run_1", 1)),
		testNow(), "k1", "fp-1"); err != nil {
		t.Fatal(err)
	}
	// Corrupt a derived index: the rebuild must reconstruct it from rows.
	snapshot.VersionIndex = map[string]int64{}
	snapshot.TrajectoryIDs = map[string]int64{}
	if err := RebuildTrajectoryIndexes(&snapshot); err != nil {
		t.Fatalf("rebuild from a valid ledger must succeed: %v", err)
	}
	if len(snapshot.VersionIndex) != 1 || len(snapshot.TrajectoryIDs) != 1 {
		t.Fatalf("indexes were not rebuilt: %+v", snapshot.VersionIndex)
	}

	snapshot.Trajectory[0].Payload["gravity"] = 1.2
	if err := RebuildTrajectoryIndexes(&snapshot); !IsCode(err, CodeTrajectoryCorrupt) {
		t.Fatalf("tampered ledger must be reported corrupt, got %v", err)
	}
}

func TestTrajectoryEntryTouchesAndSubject(t *testing.T) {
	entry := TrajectoryEntry{Refs: []TrajectoryRef{
		NewSubjectRef(EntityRun, "run_1", 3),
		NewTrajectoryRef(EntityFormula, "formula_1"),
		NewTrajectoryRef(EntityVessel, "vessel_1"),
	}}
	subject, ok := entry.Subject()
	if !ok || subject.ID != "run_1" || subject.Version != 3 {
		t.Fatalf("subject resolution failed: %+v %v", subject, ok)
	}
	for _, kind := range []EntityKind{EntityRun, EntityFormula, EntityVessel} {
		id := map[EntityKind]string{
			EntityRun: "run_1", EntityFormula: "formula_1", EntityVessel: "vessel_1",
		}[kind]
		if !entry.Touches(kind, id) {
			t.Fatalf("entry should touch %s %s", kind, id)
		}
	}
	if entry.Touches(EntityRun, "run_other") {
		t.Fatalf("entry must not touch an unrelated id")
	}
}
