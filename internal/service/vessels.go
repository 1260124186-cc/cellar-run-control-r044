package service

import (
	"sort"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func (s *Service) CreateVessel(input CreateVesselInput) (domain.Vessel, bool, error) {
	now := s.clock.Now()
	vessel := domain.Vessel{
		Code:      domain.NormalizeVesselCode(input.Code),
		CapacityL: input.CapacityL,
		State:     domain.VesselAvailable,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := vessel.Validate(); err != nil {
		return domain.Vessel{}, false, err
	}
	id, err := generateEntityID("vessel")
	if err != nil {
		return domain.Vessel{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}
	vessel.ID = id
	fingerprint, err := fingerprintAction(domain.ActionVesselCreated, map[string]any{
		"code":            vessel.Code,
		"capacity_liters": input.CapacityL,
	})
	if err != nil {
		return domain.Vessel{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.Vessel
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayVessel(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		if _, exists := snapshot.VesselIndex[vessel.Code]; exists {
			return domain.NewError(domain.CodeVesselConflict,
				"a vessel with the same code already exists")
		}
		snapshot.Vessels[vessel.ID] = vessel
		snapshot.VesselIndex[vessel.Code] = vessel.ID
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionVesselCreated,
			OccurredAt: now,
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityVessel, vessel.ID, vessel.Version)},
			Payload: map[string]any{
				"code":            vessel.Code,
				"capacity_liters": vessel.CapacityL,
				"state":           string(vessel.State),
				"created_at":      utcStamp(now),
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = vessel
		return nil
	})
	if err != nil {
		return domain.Vessel{}, false, err
	}
	return updated, replayed, nil
}

func (s *Service) GetVessel(id string) (domain.Vessel, error) {
	var found domain.Vessel
	err := s.store.View(func(snapshot domain.Snapshot) error {
		vessel, ok := snapshot.Vessels[id]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		found = vessel
		return nil
	})
	if err != nil {
		return domain.Vessel{}, err
	}
	return found, nil
}

func (s *Service) ListVessels() ([]domain.Vessel, error) {
	var vessels []domain.Vessel
	err := s.store.View(func(snapshot domain.Snapshot) error {
		vessels = make([]domain.Vessel, 0, len(snapshot.Vessels))
		for _, vessel := range snapshot.Vessels {
			vessels = append(vessels, vessel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(vessels, func(i, j int) bool {
		if vessels[i].Code == vessels[j].Code {
			return vessels[i].ID < vessels[j].ID
		}
		return vessels[i].Code < vessels[j].Code
	})
	return vessels, nil
}

func (s *Service) FinishCleaning(
	id string,
	input FinishCleaningInput,
) (domain.Vessel, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionCleaningFinished, map[string]any{
		"vessel_id":        id,
		"expected_version": versionFingerprint(input.ExpectedVersion),
	})
	if err != nil {
		return domain.Vessel{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.Vessel
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayVessel(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		vessel, ok := snapshot.Vessels[id]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		if err := s.checkExpected(vessel.Version, input.ExpectedVersion); err != nil {
			return err
		}
		if vessel.ActiveRunID == nil {
			return domain.NewError(domain.CodeVesselState,
				"cleaning vessel has no associated run")
		}
		run, ok := snapshot.Runs[*vessel.ActiveRunID]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound,
				"associated run was not found")
		}
		if run.State != domain.RunCompleted && run.State != domain.RunAborted {
			return domain.NewError(domain.CodeRunState,
				"associated run is still open")
		}
		now := s.clock.Now()
		if err := vessel.FinishCleaning(now); err != nil {
			return err
		}
		snapshot.Vessels[id] = vessel
		runID := run.ID
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionCleaningFinished,
			OccurredAt: now,
			Refs: []domain.TrajectoryRef{
				domain.NewSubjectRef(domain.EntityVessel, id, vessel.Version),
				domain.NewTrajectoryRef(domain.EntityRun, runID),
				domain.NewTrajectoryRef(domain.EntityFormula, run.FormulaID),
			},
			Payload: map[string]any{
				"vessel_state": string(vessel.State),
				"run_state":    string(run.State),
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = vessel
		return nil
	})
	if err != nil {
		return domain.Vessel{}, false, err
	}
	return updated, replayed, nil
}

func (s *Service) RetireVessel(id string, input VersionInput) (domain.Vessel, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionVesselRetired, map[string]any{
		"vessel_id":        id,
		"expected_version": versionFingerprint(input.ExpectedVersion),
	})
	if err != nil {
		return domain.Vessel{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.Vessel
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayVessel(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		vessel, ok := snapshot.Vessels[id]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		if err := s.checkExpected(vessel.Version, input.ExpectedVersion); err != nil {
			return err
		}
		now := s.clock.Now()
		if err := vessel.Retire(now); err != nil {
			return err
		}
		snapshot.Vessels[id] = vessel
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionVesselRetired,
			OccurredAt: now,
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityVessel, id, vessel.Version)},
			Payload: map[string]any{
				"vessel_state": string(vessel.State),
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = vessel
		return nil
	})
	if err != nil {
		return domain.Vessel{}, false, err
	}
	return updated, replayed, nil
}
