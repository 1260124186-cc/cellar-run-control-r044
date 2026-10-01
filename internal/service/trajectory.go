package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// canonicalJSON encodes a value with every object key sorted recursively, so
// request fingerprints do not depend on Go map iteration order.
func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(canonicalizeValue(decoded))
}

func canonicalizeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ordered := make([][2]any, 0, len(keys))
		for _, key := range keys {
			ordered = append(ordered, [2]any{key, canonicalizeValue(typed[key])})
		}
		return ordered
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = canonicalizeValue(item)
		}
		return cloned
	default:
		return value
	}
}

// fingerprintAction hashes the closed set of request-controlled values that
// define one action. Server-generated ids and timestamps are intentionally
// excluded so a genuine retry of the same request produces the same
// fingerprint, while a reused key with different parameters does not.
func fingerprintAction(action domain.TrajectoryAction, params map[string]any) (string, error) {
	body := map[string]any{"__action": string(action)}
	for key, value := range params {
		body[key] = value
	}
	raw, err := canonicalJSON(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// replayOutcome carries what a service method needs to answer a retried
// request: the already-committed ledger entry and a flag to skip mutation.
type replayOutcome struct {
	entry    domain.TrajectoryEntry
	replayed bool
}

// beginIdempotent must be called first inside a store mutation closure. When
// the key is empty it is a plain request. When the key is known it verifies
// the same action/parameters and returns the original entry for replay.
func beginIdempotent(
	snapshot *domain.Snapshot,
	key string,
	fingerprint string,
) (replayOutcome, error) {
	if key == "" {
		return replayOutcome{}, nil
	}
	record, found := snapshot.IdempotencyByKey[key]
	if !found {
		return replayOutcome{}, nil
	}
	if record.Fingerprint != fingerprint {
		return replayOutcome{}, domain.NewError(domain.CodeIdempotencyConflict,
			"the idempotency key was already used with different parameters")
	}
	if record.EntrySequence < 1 || record.EntrySequence > int64(len(snapshot.Trajectory)) {
		return replayOutcome{}, domain.NewError(domain.CodeTrajectoryCorrupt,
			"idempotency record points outside the trajectory ledger")
	}
	return replayOutcome{
		entry:    snapshot.Trajectory[record.EntrySequence-1].Clone(),
		replayed: true,
	}, nil
}

// appendEntry seals a ledger row onto the snapshot inside the open business
// transaction. The caller has already applied the business mutation, so the
// row and the new aggregate state commit or roll back together.
func appendEntry(
	snapshot *domain.Snapshot,
	draft domain.TrajectoryDraft,
	now time.Time,
	idempotencyKey, fingerprint string,
) (domain.TrajectoryEntry, error) {
	return domain.AppendTrajectory(snapshot, draft, now, idempotencyKey, fingerprint)
}

// replayFormula returns the current state of the formula recorded by a
// replayed entry. The current state is returned on purpose: aggregates remain
// queryable after later actions, while no second ledger row is produced.
func replayFormula(snapshot *domain.Snapshot, entry domain.TrajectoryEntry) (domain.Formula, error) {
	subject, ok := entry.Subject()
	if !ok || subject.Kind != domain.EntityFormula {
		return domain.Formula{}, domain.NewError(domain.CodeTrajectoryCorrupt,
			"replayed entry does not point to a formula")
	}
	formula, found := snapshot.Formulas[subject.ID]
	if !found {
		return domain.Formula{}, domain.NewError(domain.CodeFormulaNotFound, "formula was not found")
	}
	return formula.Clone(), nil
}

func replayVessel(snapshot *domain.Snapshot, entry domain.TrajectoryEntry) (domain.Vessel, error) {
	subject, ok := entry.Subject()
	if !ok || subject.Kind != domain.EntityVessel {
		return domain.Vessel{}, domain.NewError(domain.CodeTrajectoryCorrupt,
			"replayed entry does not point to a vessel")
	}
	vessel, found := snapshot.Vessels[subject.ID]
	if !found {
		return domain.Vessel{}, domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
	}
	return vessel, nil
}

func replayRun(snapshot *domain.Snapshot, entry domain.TrajectoryEntry) (domain.FermentationRun, error) {
	subject, ok := entry.Subject()
	if !ok || subject.Kind != domain.EntityRun {
		return domain.FermentationRun{}, domain.NewError(domain.CodeTrajectoryCorrupt,
			"replayed entry does not point to a run")
	}
	run, found := snapshot.Runs[subject.ID]
	if !found {
		return domain.FermentationRun{}, domain.NewError(domain.CodeRunNotFound, "run was not found")
	}
	return run.Clone(), nil
}

// vesselForReplay resolves the vessel a run-scoped entry touched, so a
// replayed run/vessel pair response still carries both aggregates.
func vesselForReplay(snapshot *domain.Snapshot, entry domain.TrajectoryEntry) (domain.Vessel, bool) {
	for _, ref := range entry.Refs {
		if ref.Kind == domain.EntityVessel {
			if vessel, found := snapshot.Vessels[ref.ID]; found {
				return vessel, true
			}
		}
	}
	return domain.Vessel{}, false
}

// utcStamp renders a timestamp as a deterministic UTC RFC3339Nano string for
// ledger payloads.
func utcStamp(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func stepsPayload(steps []domain.FormulaStep) []any {
	rendered := make([]any, 0, len(steps))
	for _, step := range steps {
		rendered = append(rendered, map[string]any{
			"sequence":      step.Sequence,
			"label":         step.Label,
			"temperature_c": step.TemperatureC,
			"hold_minutes":  step.HoldMinutes,
		})
	}
	return rendered
}

func metricsPayload(metrics *domain.RunMetrics) any {
	if metrics == nil {
		return nil
	}
	return map[string]any{
		"final_gravity":                metrics.FinalGravity,
		"apparent_attenuation_percent": metrics.ApparentAttenuation,
		"estimated_alcohol_abv":        metrics.EstimatedAlcoholABV,
		"observation_count":            metrics.ObservationCount,
	}
}
