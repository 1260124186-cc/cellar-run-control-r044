package service

import (
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func (s *Service) AppendObservation(
	id string,
	input ObservationInput,
) (domain.FermentationRun, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionObservationRecorded, map[string]any{
		"run_id":        id,
		"sequence":      input.Sequence,
		"gravity":       input.Gravity,
		"temperature_c": input.TemperatureC,
		"observed_at":   utcStamp(input.ObservedAt),
		"note":          input.Note,
	})
	if err != nil {
		return domain.FermentationRun{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.FermentationRun
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayRun(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		now := s.clock.Now()
		observation := domain.GravityObservation{
			RunID:        run.ID,
			Sequence:     input.Sequence,
			Gravity:      input.Gravity,
			TemperatureC: input.TemperatureC,
			ObservedAt:   input.ObservedAt.UTC(),
			Note:         input.Note,
			ReceivedAt:   now,
		}
		if err := run.AddObservation(observation); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionObservationRecorded,
			OccurredAt: now,
			Refs: []domain.TrajectoryRef{
				domain.NewSubjectRef(domain.EntityRun, id, run.Version),
				domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
			},
			Payload: map[string]any{
				"observation_sequence": observation.Sequence,
				"gravity":              observation.Gravity,
				"temperature_c":        observation.TemperatureC,
				"observed_at":          utcStamp(observation.ObservedAt),
				"received_at":          utcStamp(now),
				"note":                 observation.Note,
				"run_state":            string(run.State),
			},
		}
		if run.VesselID != nil {
			draft.Refs = append(draft.Refs,
				domain.NewTrajectoryRef(domain.EntityVessel, *run.VesselID))
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, false, err
	}
	return updated, replayed, nil
}

func (s *Service) BeginConditioning(
	id string,
	input VersionInput,
) (domain.FermentationRun, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionConditioningStarted, map[string]any{
		"run_id":           id,
		"expected_version": versionFingerprint(input.ExpectedVersion),
	})
	if err != nil {
		return domain.FermentationRun{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.FermentationRun
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayRun(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		if err := s.checkExpected(run.Version, input.ExpectedVersion); err != nil {
			return err
		}
		now := s.clock.Now()
		if err := run.BeginConditioning(now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionConditioningStarted,
			OccurredAt: now,
			Refs: []domain.TrajectoryRef{
				domain.NewSubjectRef(domain.EntityRun, id, run.Version),
				domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
			},
			Payload: map[string]any{
				"run_state":         string(run.State),
				"observation_count": len(run.Observations),
			},
		}
		if run.VesselID != nil {
			draft.Refs = append(draft.Refs,
				domain.NewTrajectoryRef(domain.EntityVessel, *run.VesselID))
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, false, err
	}
	return updated, replayed, nil
}

func (s *Service) CompleteRun(
	id string,
	input VersionInput,
) (domain.FermentationRun, domain.Vessel, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionRunCompleted, map[string]any{
		"run_id":           id,
		"expected_version": versionFingerprint(input.ExpectedVersion),
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updatedRun domain.FermentationRun
	var updatedVessel domain.Vessel
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updatedRun, err = replayRun(snapshot, replay.entry)
			if err != nil {
				return err
			}
			if vessel, found := vesselForReplay(snapshot, replay.entry); found {
				updatedVessel = vessel
			}
			replayed = true
			return nil
		}
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		if err := s.checkExpected(run.Version, input.ExpectedVersion); err != nil {
			return err
		}
		formula, ok := snapshot.Formulas[run.FormulaID]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "attached formula was not found")
		}
		now := s.clock.Now()
		if err := run.Complete(formula.TargetOriginalGravity, now); err != nil {
			return err
		}
		if run.VesselID == nil {
			return domain.NewError(domain.CodeRunState, "run has no vessel")
		}
		vessel, ok := snapshot.Vessels[*run.VesselID]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		if err := vessel.StartCleaning(run.ID, now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		snapshot.Vessels[vessel.ID] = vessel
		runEntry := domain.TrajectoryDraft{
			Action:     domain.ActionRunCompleted,
			OccurredAt: now,
			Refs: []domain.TrajectoryRef{
				domain.NewSubjectRef(domain.EntityRun, id, run.Version),
				domain.NewTrajectoryRef(domain.EntityVessel, vessel.ID),
				domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
			},
			Payload: map[string]any{
				"run_state":         string(run.State),
				"ended_at":          utcStamp(*run.EndedAt),
				"vessel_id":         vessel.ID,
				"vessel_state":      string(vessel.State),
				"vessel_outcome":    "cleaning",
				"observation_count": len(run.Observations),
				"metrics":           metricsPayload(run.Metrics),
			},
		}
		if _, err := appendEntry(snapshot, runEntry, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		if err := appendCleaningStarted(snapshot, vessel, run, now, domain.ActionRunCompleted); err != nil {
			return err
		}
		updatedRun = run.Clone()
		updatedVessel = vessel
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, false, err
	}
	return updatedRun, updatedVessel, replayed, nil
}

// appendCleaningStarted records the explicit vessel state change into
// cleaning. It is sealed in the same open transaction as the run effect that
// caused it, so the completion fact and the cleaning fact commit together.
// The vessel row owns no idempotency key: the caller's key already guards the
// whole operation, and a replayed request is intercepted before either row is
// sealed again.
func appendCleaningStarted(
	snapshot *domain.Snapshot,
	vessel domain.Vessel,
	run domain.FermentationRun,
	now time.Time,
	trigger domain.TrajectoryAction,
) error {
	draft := domain.TrajectoryDraft{
		Action:     domain.ActionVesselCleaning,
		OccurredAt: now,
		Refs: []domain.TrajectoryRef{
			domain.NewSubjectRef(domain.EntityVessel, vessel.ID, vessel.Version),
			domain.NewTrajectoryRef(domain.EntityRun, run.ID),
			domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
		},
		Payload: map[string]any{
			"vessel_state": string(vessel.State),
			"run_state":    string(run.State),
			"trigger":      string(trigger),
		},
	}
	_, err := appendEntry(snapshot, draft, now, "", "")
	return err
}

// appendReservationReleased records a vessel returning to available when a run
// is aborted before fermentation. It is appended in the same open transaction
// as run_aborted, so the run fact and vessel release fact cannot exist on only
// one side.
func appendReservationReleased(
	snapshot *domain.Snapshot,
	vessel domain.Vessel,
	run domain.FermentationRun,
	now time.Time,
) error {
	draft := domain.TrajectoryDraft{
		Action:     domain.ActionVesselReleased,
		OccurredAt: now,
		Refs: []domain.TrajectoryRef{
			domain.NewSubjectRef(domain.EntityVessel, vessel.ID, vessel.Version),
			domain.NewTrajectoryRef(domain.EntityRun, run.ID),
			domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
		},
		Payload: map[string]any{
			"vessel_state": string(vessel.State),
			"run_state":    string(run.State),
			"trigger":      string(domain.ActionRunAborted),
		},
	}
	_, err := appendEntry(snapshot, draft, now, "", "")
	return err
}
