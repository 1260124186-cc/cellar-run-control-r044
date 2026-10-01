package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// EntityKind identifies the kind of subject a ledger entry talks about.
type EntityKind string

const (
	EntityFormula EntityKind = "formula"
	EntityVessel  EntityKind = "vessel"
	EntityRun     EntityKind = "run"
)

// TrajectoryAction is the stable machine code of one recorded business action.
// The set is closed: sealing an entry with an unknown action is rejected.
type TrajectoryAction string

const (
	ActionFormulaCreated      TrajectoryAction = "formula_created"
	ActionFormulaStepsSet     TrajectoryAction = "formula_steps_set"
	ActionFormulaApproved     TrajectoryAction = "formula_approved"
	ActionFormulaRetired      TrajectoryAction = "formula_retired"
	ActionVesselCreated       TrajectoryAction = "vessel_created"
	ActionVesselReserved      TrajectoryAction = "vessel_reserved"
	ActionRunCreated          TrajectoryAction = "run_created"
	ActionFermentationStarted TrajectoryAction = "fermentation_started"
	ActionObservationRecorded TrajectoryAction = "observation_recorded"
	ActionConditioningStarted TrajectoryAction = "conditioning_started"
	ActionRunCompleted        TrajectoryAction = "run_completed"
	ActionRunAborted          TrajectoryAction = "run_aborted"
	ActionVesselCleaning      TrajectoryAction = "vessel_cleaning_started"
	ActionVesselReleased      TrajectoryAction = "vessel_reservation_released"
	ActionCleaningFinished    TrajectoryAction = "vessel_cleaning_finished"
	ActionVesselRetired       TrajectoryAction = "vessel_retired"
)

var knownTrajectoryActions = map[TrajectoryAction]bool{
	ActionFormulaCreated:      true,
	ActionFormulaStepsSet:     true,
	ActionFormulaApproved:     true,
	ActionFormulaRetired:      true,
	ActionVesselCreated:       true,
	ActionVesselReserved:      true,
	ActionRunCreated:          true,
	ActionFermentationStarted: true,
	ActionObservationRecorded: true,
	ActionConditioningStarted: true,
	ActionRunCompleted:        true,
	ActionRunAborted:          true,
	ActionVesselCleaning:      true,
	ActionVesselReleased:      true,
	ActionCleaningFinished:    true,
	ActionVesselRetired:       true,
}

var knownEntityKinds = map[EntityKind]bool{
	EntityFormula: true,
	EntityVessel:  true,
	EntityRun:     true,
}

// TrajectoryRef ties a ledger entry to one entity in the lineage graph.
//
// Kind/ID identify the entity. Role is either "subject" (the aggregate the
// action acts on) or "lineage" (a related aggregate needed to follow the
// formula -> run -> vessel chain). Version is the aggregate version produced
// by the action when this ref is the subject, otherwise zero.
type TrajectoryRef struct {
	Kind    EntityKind `json:"kind"`
	ID      string     `json:"id"`
	Role    string     `json:"role"`
	Version int64      `json:"version,omitempty"`
}

// TrajectoryEntry is one immutable row of the append-only production ledger.
//
// Sequence is gapless across the whole ledger and is the authoritative total
// order of actions. Each entry signs the previous entry's hash, so tampering
// with, deleting, or reordering any row breaks every later hash link.
type TrajectoryEntry struct {
	Sequence   int64            `json:"sequence"`
	ID         string           `json:"id"`
	Action     TrajectoryAction `json:"action"`
	OccurredAt time.Time        `json:"occurred_at"`
	RecordedAt time.Time        `json:"recorded_at"`
	Refs       []TrajectoryRef  `json:"refs"`
	Payload    map[string]any   `json:"payload,omitempty"`
	PrevHash   string           `json:"prev_hash"`
	EntryHash  string           `json:"entry_hash"`
}

// TrajectoryDraft carries everything needed to append one entry except the
// ledger-assigned sequence and hash links.
type TrajectoryDraft struct {
	Action     TrajectoryAction
	OccurredAt time.Time
	Refs       []TrajectoryRef
	Payload    map[string]any
}

// VersionIndexKey is the dedup key of "one ledger entry per aggregate
// version": subject kind + subject ID + produced version.
type VersionIndexKey struct {
	Kind    EntityKind
	ID      string
	Version int64
}

func (k VersionIndexKey) String() string {
	return string(k.Kind) + ":" + k.ID + ":" + fmt.Sprintf("%d", k.Version)
}

// IdempotencyRecord remembers the ledger entry claimed by an Idempotency-Key
// so a retried request is answered without appending a second entry.
type IdempotencyRecord struct {
	Key           string           `json:"key"`
	Action        TrajectoryAction `json:"action"`
	Fingerprint   string           `json:"fingerprint"`
	EntrySequence int64            `json:"entry_sequence"`
	CreatedAt     time.Time        `json:"created_at"`
}

// NewTrajectoryRef builds a lineage reference (no produced version).
func NewTrajectoryRef(kind EntityKind, id string) TrajectoryRef {
	return TrajectoryRef{Kind: kind, ID: id, Role: "lineage"}
}

// NewSubjectRef builds the subject reference carrying the produced version.
func NewSubjectRef(kind EntityKind, id string, version int64) TrajectoryRef {
	return TrajectoryRef{Kind: kind, ID: id, Role: "subject", Version: version}
}

func clonePayload(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	cloned := make(map[string]any, len(payload))
	for key, value := range payload {
		cloned[key] = value
	}
	return cloned
}

func cloneRefs(refs []TrajectoryRef) []TrajectoryRef {
	return append([]TrajectoryRef(nil), refs...)
}

func (e TrajectoryEntry) Clone() TrajectoryEntry {
	cloned := e
	cloned.Refs = cloneRefs(e.Refs)
	cloned.Payload = clonePayload(e.Payload)
	return cloned
}

// Subject returns the subject reference of an entry and whether one exists.
func (e TrajectoryEntry) Subject() (TrajectoryRef, bool) {
	for _, ref := range e.Refs {
		if ref.Role == "subject" {
			return ref, true
		}
	}
	return TrajectoryRef{}, false
}

// Touches reports whether the entry is linked to the given entity, either as
// its subject or through a lineage reference.
func (e TrajectoryEntry) Touches(kind EntityKind, id string) bool {
	for _, ref := range e.Refs {
		if ref.Kind == kind && ref.ID == id {
			return true
		}
	}
	return false
}

// digestCanonical is the fixed shape hashed into EntryHash. It is a separate
// unexported type so the signed field set can never drift away from the
// public entry fields.
type digestCanonical struct {
	Sequence   int64            `json:"sequence"`
	ID         string           `json:"id"`
	Action     TrajectoryAction `json:"action"`
	OccurredAt time.Time        `json:"occurred_at"`
	RecordedAt time.Time        `json:"recorded_at"`
	Refs       []TrajectoryRef  `json:"refs"`
	Payload    map[string]any   `json:"payload,omitempty"`
	PrevHash   string           `json:"prev_hash"`
}

// canonicalJSON sorts every object key recursively so the digest is
// independent of Go map iteration order and survives a JSON round trip.
func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(canonicalize(decoded))
}

func canonicalize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ordered := make([][2]any, 0, len(keys))
		for _, key := range keys {
			ordered = append(ordered, [2]any{key, canonicalize(typed[key])})
		}
		return ordered
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = canonicalize(item)
		}
		return cloned
	default:
		return value
	}
}

// digestEntry computes the signed hash for one entry. The hash covers the
// previous entry's hash, which is what makes the rows a verifiable chain.
func digestEntry(entry TrajectoryEntry) (string, error) {
	canonical := digestCanonical{
		Sequence:   entry.Sequence,
		ID:         entry.ID,
		Action:     entry.Action,
		OccurredAt: entry.OccurredAt.UTC(),
		RecordedAt: entry.RecordedAt.UTC(),
		Refs:       cloneRefs(entry.Refs),
		Payload:    clonePayload(entry.Payload),
		PrevHash:   entry.PrevHash,
	}
	raw, err := canonicalJSON(canonical)
	if err != nil {
		return "", fmt.Errorf("encode trajectory entry: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// SealTrajectory assigns sequence and hash links to a validated draft.
func SealTrajectory(
	draft TrajectoryDraft,
	sequence int64,
	recordedAt time.Time,
	prevHash string,
	newID func() (string, error),
) (TrajectoryEntry, error) {
	if sequence < 1 {
		return TrajectoryEntry{}, fmt.Errorf("trajectory sequence must begin at 1")
	}
	if !knownTrajectoryActions[draft.Action] {
		return TrajectoryEntry{}, fmt.Errorf("unknown trajectory action %q", draft.Action)
	}
	if draft.OccurredAt.IsZero() {
		return TrajectoryEntry{}, fmt.Errorf("trajectory entry requires an occurrence time")
	}
	if recordedAt.IsZero() {
		return TrajectoryEntry{}, fmt.Errorf("trajectory entry requires a record time")
	}
	subjectCount := 0
	for _, ref := range draft.Refs {
		if !knownEntityKinds[ref.Kind] {
			return TrajectoryEntry{}, fmt.Errorf("trajectory entry references unknown entity kind %q", ref.Kind)
		}
		if ref.ID == "" {
			return TrajectoryEntry{}, fmt.Errorf("trajectory entry reference requires an entity id")
		}
		if ref.Role == "subject" {
			subjectCount++
			if ref.Version < 1 {
				return TrajectoryEntry{}, fmt.Errorf("trajectory subject reference requires a positive version")
			}
		}
	}
	if subjectCount != 1 {
		return TrajectoryEntry{}, fmt.Errorf("trajectory entry requires exactly one subject reference")
	}
	id, err := newID()
	if err != nil {
		return TrajectoryEntry{}, err
	}
	entry := TrajectoryEntry{
		Sequence:   sequence,
		ID:         id,
		Action:     draft.Action,
		OccurredAt: draft.OccurredAt.UTC(),
		RecordedAt: recordedAt.UTC(),
		Refs:       cloneRefs(draft.Refs),
		Payload:    clonePayload(draft.Payload),
		PrevHash:   prevHash,
	}
	digest, err := digestEntry(entry)
	if err != nil {
		return TrajectoryEntry{}, err
	}
	entry.EntryHash = digest
	return entry, nil
}

// VerifyTrajectory replays the whole ledger: sequences must be gapless, ids
// unique, and every entry hash must sign the previous hash and its own bytes.
// It is used at process startup to reject a tampered or truncated ledger.
func VerifyTrajectory(entries []TrajectoryEntry) error {
	prevHash := ""
	seenIDs := make(map[string]int64, len(entries))
	for index, entry := range entries {
		expected := int64(index + 1)
		if entry.Sequence != expected {
			return fmt.Errorf("trajectory sequence %d is not contiguous at position %d", entry.Sequence, expected)
		}
		if existing, found := seenIDs[entry.ID]; found {
			return fmt.Errorf("trajectory entry id %q repeats sequences %d and %d",
				entry.ID, existing, entry.Sequence)
		}
		seenIDs[entry.ID] = entry.Sequence
		if entry.PrevHash != prevHash {
			return fmt.Errorf("trajectory entry %d breaks the previous hash link", entry.Sequence)
		}
		digest, err := digestEntry(entry)
		if err != nil {
			return fmt.Errorf("trajectory entry %d cannot be verified: %w", entry.Sequence, err)
		}
		if digest != entry.EntryHash {
			return fmt.Errorf("trajectory entry %d hash does not match its contents", entry.Sequence)
		}
		prevHash = entry.EntryHash
	}
	return nil
}

// TrajectoryVersionKey extracts the dedup key of an entry, or false when the
// entry has no subject version (it never should after sealing).
func TrajectoryVersionKey(entry TrajectoryEntry) (VersionIndexKey, bool) {
	subject, ok := entry.Subject()
	if !ok || subject.Version < 1 {
		return VersionIndexKey{}, false
	}
	return VersionIndexKey{Kind: subject.Kind, ID: subject.ID, Version: subject.Version}, true
}

// AppendTrajectory seals one draft onto the snapshot ledger together with its
// dedup indexes. It is a pure in-memory mutation: durability and mutual
// exclusion are owned by storage, which commits the whole snapshot atomically.
//
// A non-empty idempotencyKey claims the new entry so a retried request can be
// recognized. A subject version that already has an entry is refused, which
// guarantees "one business version = one ledger row" even without a key.
func AppendTrajectory(
	snapshot *Snapshot,
	draft TrajectoryDraft,
	recordedAt time.Time,
	idempotencyKey, fingerprint string,
) (TrajectoryEntry, error) {
	if snapshot.Trajectory == nil {
		snapshot.Trajectory = []TrajectoryEntry{}
	}
	if snapshot.TrajectoryIDs == nil {
		snapshot.TrajectoryIDs = map[string]int64{}
	}
	if snapshot.VersionIndex == nil {
		snapshot.VersionIndex = map[string]int64{}
	}
	if snapshot.IdempotencyByKey == nil {
		snapshot.IdempotencyByKey = map[string]IdempotencyRecord{}
	}
	if idempotencyKey != "" {
		if _, claimed := snapshot.IdempotencyByKey[idempotencyKey]; claimed {
			return TrajectoryEntry{}, NewError(CodeIdempotencyConflict,
				"the idempotency key has already been recorded")
		}
	}
	for _, ref := range draft.Refs {
		if ref.Role == "subject" && ref.Version >= 1 {
			key := (VersionIndexKey{Kind: ref.Kind, ID: ref.ID, Version: ref.Version}).String()
			if _, exists := snapshot.VersionIndex[key]; exists {
				return TrajectoryEntry{}, NewError(CodeTrajectoryConflict,
					"a trajectory entry already exists for this aggregate version")
			}
		}
	}
	sequence := int64(len(snapshot.Trajectory) + 1)
	prevHash := ""
	if len(snapshot.Trajectory) > 0 {
		prevHash = snapshot.Trajectory[len(snapshot.Trajectory)-1].EntryHash
	}
	entry, err := SealTrajectory(draft, sequence, recordedAt, prevHash, func() (string, error) {
		return NewID("ledger")
	})
	if err != nil {
		return TrajectoryEntry{}, NewError(CodeStorage, err.Error())
	}
	snapshot.Trajectory = append(snapshot.Trajectory, entry)
	snapshot.TrajectoryIDs[entry.ID] = entry.Sequence
	if key, ok := TrajectoryVersionKey(entry); ok {
		snapshot.VersionIndex[key.String()] = entry.Sequence
	}
	if idempotencyKey != "" {
		snapshot.IdempotencyByKey[idempotencyKey] = IdempotencyRecord{
			Key:           idempotencyKey,
			Action:        entry.Action,
			Fingerprint:   fingerprint,
			EntrySequence: entry.Sequence,
			CreatedAt:     recordedAt.UTC(),
		}
	}
	return entry, nil
}

// RebuildTrajectoryIndexes validates the hash chain of an already-loaded
// snapshot and reconstructs every derived index from the ledger itself, so a
// missing or stale index file field can never disagree with the ledger.
func RebuildTrajectoryIndexes(snapshot *Snapshot) error {
	if err := VerifyTrajectory(snapshot.Trajectory); err != nil {
		return NewError(CodeTrajectoryCorrupt, err.Error())
	}
	idIndex := make(map[string]int64, len(snapshot.Trajectory))
	versionIndex := make(map[string]int64, len(snapshot.Trajectory))
	for _, entry := range snapshot.Trajectory {
		if _, dup := idIndex[entry.ID]; dup {
			return NewError(CodeTrajectoryCorrupt, "trajectory id index contains a duplicate")
		}
		idIndex[entry.ID] = entry.Sequence
		if key, ok := TrajectoryVersionKey(entry); ok {
			if existing, dup := versionIndex[key.String()]; dup {
				return NewError(CodeTrajectoryCorrupt, fmt.Sprintf(
					"trajectory versions %d and %d repeat one aggregate version", existing, entry.Sequence))
			}
			versionIndex[key.String()] = entry.Sequence
		}
	}
	snapshot.TrajectoryIDs = idIndex
	snapshot.VersionIndex = versionIndex
	if snapshot.IdempotencyByKey == nil {
		snapshot.IdempotencyByKey = map[string]IdempotencyRecord{}
	}
	for key, record := range snapshot.IdempotencyByKey {
		if record.Key != key || record.EntrySequence < 1 ||
			record.EntrySequence > int64(len(snapshot.Trajectory)) {
			return NewError(CodeTrajectoryCorrupt,
				"idempotency index points outside the trajectory ledger")
		}
		if record.Fingerprint == "" {
			return NewError(CodeTrajectoryCorrupt,
				"idempotency index record is missing its fingerprint")
		}
		if record.Action != snapshot.Trajectory[record.EntrySequence-1].Action {
			return NewError(CodeTrajectoryCorrupt,
				"idempotency index does not match its ledger entry")
		}
	}
	return nil
}
