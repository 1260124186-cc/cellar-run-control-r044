// Package ledger contains the append-only production trajectory kernel.
//
// A trajectory log records one immutable entry for every key production
// action (formula release, vessel transfer, fermentation observation,
// completion, cleaning, and so on). Entries are assigned gapless global
// sequence numbers in commit order and can be read back by entity lineage
// or by an occurrence-time filter. The log is never mutated in place:
// appends are the only supported write.
package ledger

import (
	"errors"
	"fmt"
	"time"
)

// EntityType identifies the kind of production entity an entry belongs to.
type EntityType string

const (
	EntityFormula EntityType = "formula"
	EntityVessel  EntityType = "vessel"
	EntityRun     EntityType = "run"
)

// Action names a single key production action. Every action belongs to
// exactly one subject entity type.
type Action string

const (
	ActionFormulaCreated   Action = "formula.created"
	ActionFormulaStepsSet  Action = "formula.steps_set"
	ActionFormulaApproved  Action = "formula.approved"
	ActionFormulaRetired   Action = "formula.retired"
	ActionVesselCreated    Action = "vessel.created"
	ActionVesselReserved   Action = "vessel.reserved"
	ActionVesselRunStarted Action = "vessel.run_started"
	ActionVesselCleaning   Action = "vessel.cleaning_started"
	ActionVesselReleased   Action = "vessel.released"
	ActionVesselCleaned    Action = "vessel.cleaning_finished"
	ActionVesselRetired    Action = "vessel.retired"
	ActionRunCreated       Action = "run.created"
	ActionRunReserved      Action = "run.reserved"
	ActionRunStarted       Action = "run.started"
	ActionObservationAdded Action = "run.observation_added"
	ActionRunConditioning  Action = "run.conditioning_started"
	ActionRunCompleted     Action = "run.completed"
	ActionRunAborted       Action = "run.aborted"
)

// actionSubjects maps every known action to the entity type it mutates.
var actionSubjects = map[Action]EntityType{
	ActionFormulaCreated:   EntityFormula,
	ActionFormulaStepsSet:  EntityFormula,
	ActionFormulaApproved:  EntityFormula,
	ActionFormulaRetired:   EntityFormula,
	ActionVesselCreated:    EntityVessel,
	ActionVesselReserved:   EntityVessel,
	ActionVesselRunStarted: EntityVessel,
	ActionVesselCleaning:   EntityVessel,
	ActionVesselReleased:   EntityVessel,
	ActionVesselCleaned:    EntityVessel,
	ActionVesselRetired:    EntityVessel,
	ActionRunCreated:       EntityRun,
	ActionRunReserved:      EntityRun,
	ActionRunStarted:       EntityRun,
	ActionObservationAdded: EntityRun,
	ActionRunConditioning:  EntityRun,
	ActionRunCompleted:     EntityRun,
	ActionRunAborted:       EntityRun,
}

// Ref points at a production entity participating in an entry. Code carries
// the human-facing formula name, vessel code, or run code when available.
type Ref struct {
	Type EntityType `json:"type"`
	ID   string     `json:"id"`
	Code string     `json:"code,omitempty"`
}

// Equal reports whether two references point at the same entity.
func (r Ref) Equal(other Ref) bool {
	return r.Type == other.Type && r.ID == other.ID
}

// Entry is one immutable row in the production trajectory.
type Entry struct {
	// Sequence is the gapless global position assigned at append time.
	Sequence int64 `json:"sequence"`
	// Action is the stable machine name of the recorded action.
	Action Action `json:"action"`
	// OccurredAt is when the business action took effect.
	OccurredAt time.Time `json:"occurred_at"`
	// Subject is the entity whose version moved because of this action.
	Subject Ref `json:"subject"`
	// EntityVersion is the subject aggregate version produced by the action.
	// (subject type, subject id, entity version) is the idempotency key.
	EntityVersion int64 `json:"entity_version"`
	// FromState and ToState record the subject lifecycle transition. They
	// are equal for actions that append data without changing state.
	FromState string `json:"from_state,omitempty"`
	ToState   string `json:"to_state,omitempty"`
	// Lineage links the other formula/vessel/run entities involved so an
	// entry can be read from any side of the production bloodline.
	Lineage []Ref `json:"lineage,omitempty"`
	// Detail carries action-specific facts (gravity, metrics, counts).
	Detail map[string]any `json:"detail,omitempty"`
}

// Clone returns a deep copy of the entry.
func (e Entry) Clone() Entry {
	cloned := e
	if e.Lineage != nil {
		cloned.Lineage = append([]Ref(nil), e.Lineage...)
	}
	if e.Detail != nil {
		cloned.Detail = make(map[string]any, len(e.Detail))
		for key, value := range e.Detail {
			cloned.Detail[key] = value
		}
	}
	return cloned
}

// ErrorKind classifies a ledger failure.
type ErrorKind string

const (
	KindInvalidEntry ErrorKind = "invalid_entry"
	KindConflict     ErrorKind = "entry_conflict"
	KindCorruptLog   ErrorKind = "corrupt_log"
)

// Error is the only error type returned by the ledger package.
type Error struct {
	Kind    ErrorKind
	Message string
	Field   string
}

func (e *Error) Error() string {
	if e.Field == "" {
		return string(e.Kind) + ": " + e.Message
	}
	return string(e.Kind) + ": " + e.Message + " (" + e.Field + ")"
}

func entryError(message, field string) *Error {
	return &Error{Kind: KindInvalidEntry, Message: message, Field: field}
}

func corruptError(message string) *Error {
	return &Error{Kind: KindCorruptLog, Message: message}
}

// IsKind reports whether err is a ledger error of the given kind.
func IsKind(err error, kind ErrorKind) bool {
	var ledgerErr *Error
	return errors.As(err, &ledgerErr) && ledgerErr.Kind == kind
}

const (
	maxIDRunes      = 64
	maxCodeRunes    = 120
	maxStateRunes   = 32
	maxLineageRefs  = 8
	maxDetailKeys   = 16
	maxDetailKeyLen = 64
)

func validEntityType(value EntityType) bool {
	return value == EntityFormula || value == EntityVessel || value == EntityRun
}

func validateRef(ref Ref, field string) error {
	if !validEntityType(ref.Type) {
		return entryError("lineage reference has an unknown entity type", field)
	}
	if ref.ID == "" || len([]rune(ref.ID)) > maxIDRunes {
		return entryError("entity identifier is required and must not exceed 64 characters", field)
	}
	if len([]rune(ref.Code)) > maxCodeRunes {
		return entryError("entity code must not exceed 120 characters", field)
	}
	return nil
}

// validateForAppend checks a candidate entry. Sequence is assigned by the
// log and must not be set by the caller.
func (e Entry) validateForAppend() error {
	if e.Sequence != 0 {
		return entryError("sequence is assigned by the log", "sequence")
	}
	expectedSubject, known := actionSubjects[e.Action]
	if !known {
		return entryError("action is not a known trajectory action", "action")
	}
	if err := validateRef(e.Subject, "subject"); err != nil {
		return err
	}
	if e.Subject.Type != expectedSubject {
		return entryError("action does not belong to the subject entity type", "subject")
	}
	if e.EntityVersion < 1 {
		return entryError("entity version must be positive", "entity_version")
	}
	if e.OccurredAt.IsZero() {
		return entryError("occurred_at is required", "occurred_at")
	}
	if len([]rune(e.FromState)) > maxStateRunes || len([]rune(e.ToState)) > maxStateRunes {
		return entryError("state value must not exceed 32 characters", "state")
	}
	if len(e.Lineage) > maxLineageRefs {
		return entryError("lineage must contain at most 8 references", "lineage")
	}
	seen := map[string]bool{entityKey(e.Subject.Type, e.Subject.ID): true}
	for index, ref := range e.Lineage {
		field := fmt.Sprintf("lineage[%d]", index)
		if err := validateRef(ref, field); err != nil {
			return err
		}
		key := entityKey(ref.Type, ref.ID)
		if seen[key] {
			return entryError("lineage references must be distinct and exclude the subject", field)
		}
		seen[key] = true
	}
	if len(e.Detail) > maxDetailKeys {
		return entryError("detail must contain at most 16 keys", "detail")
	}
	for key := range e.Detail {
		if key == "" || len([]rune(key)) > maxDetailKeyLen {
			return entryError("detail keys must be 1 to 64 characters", "detail")
		}
	}
	return nil
}

// normalize canonicalizes an entry before it is stored.
func (e *Entry) normalize() {
	e.OccurredAt = e.OccurredAt.UTC()
	if len(e.Lineage) == 0 {
		e.Lineage = nil
	}
	if len(e.Detail) == 0 {
		e.Detail = nil
	}
}

// sameBusiness reports whether two entries describe the same action for the
// same idempotency key. Sequence is intentionally ignored.
func (e Entry) sameBusiness(other Entry) bool {
	if e.Action != other.Action ||
		!e.OccurredAt.Equal(other.OccurredAt) ||
		!e.Subject.Equal(other.Subject) ||
		e.EntityVersion != other.EntityVersion ||
		e.FromState != other.FromState ||
		e.ToState != other.ToState ||
		!refsEqual(e.Lineage, other.Lineage) {
		return false
	}
	return detailsEqual(e.Detail, other.Detail)
}

func refsEqual(left, right []Ref) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func detailsEqual(left, right map[string]any) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || value != other {
			return false
		}
	}
	return true
}
