package service

import (
	"sort"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func (s *Service) CreateRun(input CreateRunInput) (domain.FermentationRun, bool, error) {
	now := s.clock.Now()
	code := domain.NormalizeRunCode(input.Code)
	if code == "" {
		generated, err := generatedRunCode()
		if err != nil {
			return domain.FermentationRun{}, false, domain.NewError(domain.CodeStorage, err.Error())
		}
		code = generated
	}
	run := domain.FermentationRun{
		Code:           code,
		FormulaID:      input.FormulaID,
		PlannedVolumeL: input.PlannedVolumeL,
		Notes:          input.Notes,
		State:          domain.RunPlanned,
		Observations:   []domain.GravityObservation{},
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := run.Validate(); err != nil {
		return domain.FermentationRun{}, false, err
	}
	id, err := generateEntityID("run")
	if err != nil {
		return domain.FermentationRun{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}
	run.ID = id
	fingerprint, err := fingerprintAction(domain.ActionRunCreated, map[string]any{
		"code":                  code,
		"formula_id":            input.FormulaID,
		"planned_volume_liters": input.PlannedVolumeL,
		"notes":                 input.Notes,
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
		formula, ok := snapshot.Formulas[run.FormulaID]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "formula was not found")
		}
		if formula.State != domain.FormulaApproved {
			return domain.NewError(domain.CodeFormulaState,
				"run requires an approved formula")
		}
		if _, exists := snapshot.RunIndex[run.Code]; exists {
			return domain.NewError(domain.CodeRunConflict,
				"a run with the same code already exists")
		}
		snapshot.Runs[run.ID] = run.Clone()
		snapshot.RunIndex[run.Code] = run.ID
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionRunCreated,
			OccurredAt: now,
			Refs: []domain.TrajectoryRef{
				domain.NewSubjectRef(domain.EntityRun, run.ID, run.Version),
				domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
			},
			Payload: map[string]any{
				"code":                  run.Code,
				"formula_id":            run.FormulaID,
				"planned_volume_liters": run.PlannedVolumeL,
				"notes":                 run.Notes,
				"run_state":             string(run.State),
				"created_at":            utcStamp(now),
			},
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

func (s *Service) ReserveRun(
	id string,
	input ReserveRunInput,
) (domain.FermentationRun, domain.Vessel, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionVesselReserved, map[string]any{
		"run_id":           id,
		"expected_version": versionFingerprint(input.ExpectedVersion),
		"vessel_id":        input.VesselID,
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
			} else {
				return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
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
		vessel, ok := snapshot.Vessels[input.VesselID]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		if run.PlannedVolumeL > vessel.CapacityL {
			return domain.FieldError(domain.CodeInvalidInput,
				"planned_volume_liters", "exceeds vessel capacity")
		}
		now := s.clock.Now()
		if err := vessel.Reserve(run.ID, now); err != nil {
			return err
		}
		if err := run.ReserveVessel(vessel.ID, now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		snapshot.Vessels[vessel.ID] = vessel
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionVesselReserved,
			OccurredAt: now,
			Refs: []domain.TrajectoryRef{
				domain.NewSubjectRef(domain.EntityRun, id, run.Version),
				domain.NewTrajectoryRef(domain.EntityVessel, vessel.ID),
				domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
			},
			Payload: map[string]any{
				"vessel_id":             vessel.ID,
				"vessel_version":        vessel.Version,
				"vessel_state":          string(vessel.State),
				"run_state":             string(run.State),
				"planned_volume_liters": run.PlannedVolumeL,
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
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

func (s *Service) StartRun(
	id string,
	input VersionInput,
) (domain.FermentationRun, domain.Vessel, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionFermentationStarted, map[string]any{
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
			} else {
				return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
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
		if run.VesselID == nil {
			return domain.NewError(domain.CodeRunState, "run has no vessel")
		}
		vessel, ok := snapshot.Vessels[*run.VesselID]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		now := s.clock.Now()
		if err := vessel.StartRun(run.ID, now); err != nil {
			return err
		}
		if err := run.StartFermentation(now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		snapshot.Vessels[vessel.ID] = vessel
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionFermentationStarted,
			OccurredAt: now,
			Refs: []domain.TrajectoryRef{
				domain.NewSubjectRef(domain.EntityRun, id, run.Version),
				domain.NewTrajectoryRef(domain.EntityVessel, vessel.ID),
				domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
			},
			Payload: map[string]any{
				"vessel_id":      vessel.ID,
				"vessel_version": vessel.Version,
				"vessel_state":   string(vessel.State),
				"run_state":      string(run.State),
				"started_at":     utcStamp(*run.StartedAt),
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
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

func (s *Service) AbortRun(
	id string,
	input VersionInput,
) (domain.FermentationRun, domain.Vessel, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionRunAborted, map[string]any{
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
		hadStarted := run.HadStarted()
		now := s.clock.Now()
		if err := run.Abort(now); err != nil {
			return err
		}
		refs := []domain.TrajectoryRef{
			domain.NewSubjectRef(domain.EntityRun, id, run.Version),
			domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
		}
		payload := map[string]any{
			"run_state":   string(run.State),
			"ended_at":    utcStamp(*run.EndedAt),
			"had_started": hadStarted,
		}
		if run.VesselID != nil {
			vessel, ok := snapshot.Vessels[*run.VesselID]
			if !ok {
				return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
			}
			if hadStarted {
				if err := vessel.StartCleaning(run.ID, now); err != nil {
					return err
				}
				payload["vessel_outcome"] = "cleaning"
			} else {
				if err := vessel.ReleaseReservation(run.ID, now); err != nil {
					return err
				}
				payload["vessel_outcome"] = "released"
			}
			snapshot.Vessels[vessel.ID] = vessel
			updatedVessel = vessel
			refs = append(refs, domain.NewTrajectoryRef(domain.EntityVessel, vessel.ID))
			payload["vessel_id"] = vessel.ID
			payload["vessel_version"] = vessel.Version
			payload["vessel_state"] = string(vessel.State)
		} else {
			payload["vessel_outcome"] = "none"
		}
		snapshot.Runs[id] = run.Clone()
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionRunAborted,
			OccurredAt: now,
			Refs:       refs,
			Payload:    payload,
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		if hadStarted && run.VesselID != nil {
			if err := appendCleaningStarted(snapshot, updatedVessel, run, now,
				domain.ActionRunAborted); err != nil {
				return err
			}
		}
		if !hadStarted && run.VesselID != nil {
			if err := appendReservationReleased(snapshot, updatedVessel, run, now); err != nil {
				return err
			}
		}
		updatedRun = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, false, err
	}
	return updatedRun, updatedVessel, replayed, nil
}

func (s *Service) GetRun(id string) (domain.FermentationRun, error) {
	var found domain.FermentationRun
	err := s.store.View(func(snapshot domain.Snapshot) error {
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		found = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	return found, nil
}

func (s *Service) ListRuns() ([]domain.FermentationRun, error) {
	var runs []domain.FermentationRun
	err := s.store.View(func(snapshot domain.Snapshot) error {
		runs = make([]domain.FermentationRun, 0, len(snapshot.Runs))
		for _, run := range snapshot.Runs {
			runs = append(runs, run.Clone())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Code == runs[j].Code {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].Code < runs[j].Code
	})
	return runs, nil
}
