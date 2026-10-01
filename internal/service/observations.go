package service

import (
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ledger"
)

func (s *Service) AppendObservation(
	id string,
	input ObservationInput,
) (domain.FermentationRun, error) {
	var updated domain.FermentationRun
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
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
		fromState := run.State
		if err := run.AddObservation(observation); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		updated = run.Clone()
		lineage := []ledger.Ref{formulaRef(snapshot.Formulas[run.FormulaID])}
		if run.VesselID != nil {
			lineage = append(lineage, vesselRef(snapshot.Vessels[*run.VesselID]))
		}
		detail := map[string]any{
			"sequence":      observation.Sequence,
			"gravity":       observation.Gravity,
			"temperature_c": observation.TemperatureC,
			"observed_at":   observation.ObservedAt,
		}
		if observation.Note != "" {
			detail["note"] = observation.Note
		}
		return appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionObservationAdded,
			OccurredAt:    now,
			Subject:       runRef(run),
			EntityVersion: run.Version,
			FromState:     string(fromState),
			ToState:       string(run.State),
			Lineage:       lineage,
			Detail:        detail,
		})
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	return updated, nil
}

func (s *Service) BeginConditioning(
	id string,
	input VersionInput,
) (domain.FermentationRun, error) {
	var updated domain.FermentationRun
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		if err := s.checkExpected(run.Version, input.ExpectedVersion); err != nil {
			return err
		}
		fromState := run.State
		now := s.clock.Now()
		if err := run.BeginConditioning(now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		updated = run.Clone()
		lineage := []ledger.Ref{formulaRef(snapshot.Formulas[run.FormulaID])}
		if run.VesselID != nil {
			lineage = append(lineage, vesselRef(snapshot.Vessels[*run.VesselID]))
		}
		return appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionRunConditioning,
			OccurredAt:    now,
			Subject:       runRef(run),
			EntityVersion: run.Version,
			FromState:     string(fromState),
			ToState:       string(run.State),
			Lineage:       lineage,
			Detail: map[string]any{
				"observation_count": len(run.Observations),
			},
		})
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	return updated, nil
}

func (s *Service) CompleteRun(
	id string,
	input VersionInput,
) (domain.FermentationRun, domain.Vessel, error) {
	var updatedRun domain.FermentationRun
	var updatedVessel domain.Vessel
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
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
		runFrom := run.State
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
		vesselFrom := vessel.State
		if err := vessel.StartCleaning(run.ID, now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		snapshot.Vessels[vessel.ID] = vessel
		updatedRun = run.Clone()
		updatedVessel = vessel
		detail := map[string]any{
			"observation_count": len(run.Observations),
		}
		if run.Metrics != nil {
			detail["final_gravity"] = run.Metrics.FinalGravity
			detail["apparent_attenuation_percent"] = run.Metrics.ApparentAttenuation
			detail["estimated_alcohol_abv"] = run.Metrics.EstimatedAlcoholABV
		}
		if err := appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionRunCompleted,
			OccurredAt:    now,
			Subject:       runRef(run),
			EntityVersion: run.Version,
			FromState:     string(runFrom),
			ToState:       string(run.State),
			Lineage:       []ledger.Ref{formulaRef(formula), vesselRef(vessel)},
			Detail:        detail,
		}); err != nil {
			return err
		}
		return appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionVesselCleaning,
			OccurredAt:    now,
			Subject:       vesselRef(vessel),
			EntityVersion: vessel.Version,
			FromState:     string(vesselFrom),
			ToState:       string(vessel.State),
			Lineage:       []ledger.Ref{runRef(run), formulaRef(formula)},
			Detail: map[string]any{
				"reason": "run_completed",
			},
		})
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, err
	}
	return updatedRun, updatedVessel, nil
}
