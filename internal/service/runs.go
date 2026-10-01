package service

import (
	"sort"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ledger"
)

func (s *Service) CreateRun(input CreateRunInput) (domain.FermentationRun, error) {
	now := s.clock.Now()
	code := domain.NormalizeRunCode(input.Code)
	if code == "" {
		generated, err := generatedRunCode()
		if err != nil {
			return domain.FermentationRun{}, domain.NewError(domain.CodeStorage, err.Error())
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
		return domain.FermentationRun{}, err
	}
	id, err := generateEntityID("run")
	if err != nil {
		return domain.FermentationRun{}, domain.NewError(domain.CodeStorage, err.Error())
	}
	run.ID = id

	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
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
		return appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionRunCreated,
			OccurredAt:    now,
			Subject:       runRef(run),
			EntityVersion: run.Version,
			FromState:     "",
			ToState:       string(domain.RunPlanned),
			Lineage:       []ledger.Ref{formulaRef(formula)},
			Detail: map[string]any{
				"code":                  run.Code,
				"planned_volume_liters": run.PlannedVolumeL,
			},
		})
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	return run.Clone(), nil
}

func (s *Service) ReserveRun(
	id string,
	input ReserveRunInput,
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
		vessel, ok := snapshot.Vessels[input.VesselID]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		if run.PlannedVolumeL > vessel.CapacityL {
			return domain.FieldError(domain.CodeInvalidInput,
				"planned_volume_liters", "exceeds vessel capacity")
		}
		now := s.clock.Now()
		vesselFrom := vessel.State
		runFrom := run.State
		if err := vessel.Reserve(run.ID, now); err != nil {
			return err
		}
		if err := run.ReserveVessel(vessel.ID, now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		snapshot.Vessels[vessel.ID] = vessel
		updatedRun = run.Clone()
		updatedVessel = vessel
		formula := snapshot.Formulas[run.FormulaID]
		if err := appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionVesselReserved,
			OccurredAt:    now,
			Subject:       vesselRef(vessel),
			EntityVersion: vessel.Version,
			FromState:     string(vesselFrom),
			ToState:       string(vessel.State),
			Lineage:       []ledger.Ref{runRef(run), formulaRef(formula)},
		}); err != nil {
			return err
		}
		return appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionRunReserved,
			OccurredAt:    now,
			Subject:       runRef(run),
			EntityVersion: run.Version,
			FromState:     string(runFrom),
			ToState:       string(run.State),
			Lineage:       []ledger.Ref{formulaRef(formula), vesselRef(vessel)},
			Detail: map[string]any{
				"planned_volume_liters": run.PlannedVolumeL,
			},
		})
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, err
	}
	return updatedRun, updatedVessel, nil
}

func (s *Service) StartRun(
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
		if run.VesselID == nil {
			return domain.NewError(domain.CodeRunState, "run has no vessel")
		}
		vessel, ok := snapshot.Vessels[*run.VesselID]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		now := s.clock.Now()
		vesselFrom := vessel.State
		runFrom := run.State
		if err := vessel.StartRun(run.ID, now); err != nil {
			return err
		}
		if err := run.StartFermentation(now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		snapshot.Vessels[vessel.ID] = vessel
		updatedRun = run.Clone()
		updatedVessel = vessel
		formula := snapshot.Formulas[run.FormulaID]
		if err := appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionVesselRunStarted,
			OccurredAt:    now,
			Subject:       vesselRef(vessel),
			EntityVersion: vessel.Version,
			FromState:     string(vesselFrom),
			ToState:       string(vessel.State),
			Lineage:       []ledger.Ref{runRef(run), formulaRef(formula)},
		}); err != nil {
			return err
		}
		return appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionRunStarted,
			OccurredAt:    now,
			Subject:       runRef(run),
			EntityVersion: run.Version,
			FromState:     string(runFrom),
			ToState:       string(run.State),
			Lineage:       []ledger.Ref{formulaRef(formula), vesselRef(vessel)},
		})
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, err
	}
	return updatedRun, updatedVessel, nil
}

func (s *Service) AbortRun(
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
		hadStarted := run.HadStarted()
		now := s.clock.Now()
		runFrom := run.State
		if err := run.Abort(now); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		updatedRun = run.Clone()
		formula := snapshot.Formulas[run.FormulaID]
		var vesselEntry *ledger.Entry
		if run.VesselID != nil {
			vessel, ok := snapshot.Vessels[*run.VesselID]
			if !ok {
				return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
			}
			vesselFrom := vessel.State
			vesselAction := ledger.Action("")
			if hadStarted {
				if err := vessel.StartCleaning(run.ID, now); err != nil {
					return err
				}
				vesselAction = ledger.ActionVesselCleaning
			} else {
				if err := vessel.ReleaseReservation(run.ID, now); err != nil {
					return err
				}
				vesselAction = ledger.ActionVesselReleased
			}
			snapshot.Vessels[vessel.ID] = vessel
			updatedVessel = vessel
			vesselEntry = &ledger.Entry{
				Action:        vesselAction,
				OccurredAt:    now,
				Subject:       vesselRef(vessel),
				EntityVersion: vessel.Version,
				FromState:     string(vesselFrom),
				ToState:       string(vessel.State),
				Lineage:       []ledger.Ref{runRef(run), formulaRef(formula)},
				Detail: map[string]any{
					"reason": "run_aborted",
				},
			}
		}
		lineage := []ledger.Ref{formulaRef(formula)}
		if run.VesselID != nil {
			lineage = append(lineage, vesselRef(updatedVessel))
		}
		if err := appendTrajectory(snapshot, ledger.Entry{
			Action:        ledger.ActionRunAborted,
			OccurredAt:    now,
			Subject:       runRef(run),
			EntityVersion: run.Version,
			FromState:     string(runFrom),
			ToState:       string(run.State),
			Lineage:       lineage,
			Detail: map[string]any{
				"had_started": hadStarted,
			},
		}); err != nil {
			return err
		}
		if vesselEntry != nil {
			return appendTrajectory(snapshot, *vesselEntry)
		}
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, err
	}
	return updatedRun, updatedVessel, nil
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
