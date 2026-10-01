package service

import (
	"sort"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func (s *Service) CreateFormula(input CreateFormulaInput) (domain.Formula, bool, error) {
	now := s.clock.Now()
	formula := domain.Formula{
		Name:                  input.Name,
		Style:                 input.Style,
		TargetOriginalGravity: input.TargetOriginalGravity,
		TargetFinalGravity:    input.TargetFinalGravity,
		MaxFermentationTempC:  input.MaxFermentationTempC,
		MinimumDays:           input.MinimumDays,
		MaximumDays:           input.MaximumDays,
		Steps:                 []domain.FormulaStep{},
		State:                 domain.FormulaDraft,
		Version:               1,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	formula.Normalize()
	if err := formula.Validate(); err != nil {
		return domain.Formula{}, false, err
	}
	id, err := generateEntityID("formula")
	if err != nil {
		return domain.Formula{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}
	formula.ID = id
	nameKey := domain.NameKey(formula.Name)
	fingerprint, err := fingerprintAction(domain.ActionFormulaCreated, map[string]any{
		"name":                    formula.Name,
		"style":                   formula.Style,
		"target_original_gravity": input.TargetOriginalGravity,
		"target_final_gravity":    input.TargetFinalGravity,
		"max_fermentation_temp_c": input.MaxFermentationTempC,
		"minimum_days":            input.MinimumDays,
		"maximum_days":            input.MaximumDays,
	})
	if err != nil {
		return domain.Formula{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.Formula
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayFormula(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		if _, exists := snapshot.NameIndex[nameKey]; exists {
			return domain.NewError(domain.CodeFormulaNameConflict,
				"a formula with the same normalized name already exists")
		}
		snapshot.Formulas[formula.ID] = formula
		snapshot.NameIndex[nameKey] = formula.ID
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionFormulaCreated,
			OccurredAt: now,
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityFormula, formula.ID, formula.Version)},
			Payload: map[string]any{
				"name":                    formula.Name,
				"style":                   formula.Style,
				"target_original_gravity": formula.TargetOriginalGravity,
				"target_final_gravity":    formula.TargetFinalGravity,
				"max_fermentation_temp_c": formula.MaxFermentationTempC,
				"minimum_days":            input.MinimumDays,
				"maximum_days":            input.MaximumDays,
				"created_at":              utcStamp(now),
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = formula.Clone()
		return nil
	})
	if err != nil {
		return domain.Formula{}, false, err
	}
	return updated, replayed, nil
}

func (s *Service) SetFormulaSteps(
	id string,
	input SetFormulaStepsInput,
) (domain.Formula, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionFormulaStepsSet, map[string]any{
		"formula_id":       id,
		"expected_version": versionFingerprint(input.ExpectedVersion),
		"steps":            stepsFingerprint(input.Steps),
	})
	if err != nil {
		return domain.Formula{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.Formula
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayFormula(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		formula, ok := snapshot.Formulas[id]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "formula was not found")
		}
		if err := s.checkExpected(formula.Version, input.ExpectedVersion); err != nil {
			return err
		}
		now := s.clock.Now()
		if err := formula.SetSteps(input.Steps); err != nil {
			return err
		}
		formula.UpdatedAt = now
		snapshot.Formulas[id] = formula
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionFormulaStepsSet,
			OccurredAt: now,
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityFormula, id, formula.Version)},
			Payload: map[string]any{
				"step_count": len(formula.Steps),
				"steps":      stepsPayload(formula.Steps),
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = formula.Clone()
		return nil
	})
	if err != nil {
		return domain.Formula{}, false, err
	}
	return updated, replayed, nil
}

func (s *Service) ApproveFormula(id string, input VersionInput) (domain.Formula, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionFormulaApproved, map[string]any{
		"formula_id":       id,
		"expected_version": versionFingerprint(input.ExpectedVersion),
	})
	if err != nil {
		return domain.Formula{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.Formula
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayFormula(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		formula, ok := snapshot.Formulas[id]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "formula was not found")
		}
		if err := s.checkExpected(formula.Version, input.ExpectedVersion); err != nil {
			return err
		}
		now := s.clock.Now()
		if err := formula.Approve(now); err != nil {
			return err
		}
		snapshot.Formulas[id] = formula
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionFormulaApproved,
			OccurredAt: now,
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityFormula, id, formula.Version)},
			Payload: map[string]any{
				"approved_at": utcStamp(*formula.ApprovedAt),
				"step_count":  len(formula.Steps),
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = formula.Clone()
		return nil
	})
	if err != nil {
		return domain.Formula{}, false, err
	}
	return updated, replayed, nil
}

func (s *Service) RetireFormula(id string, input VersionInput) (domain.Formula, bool, error) {
	fingerprint, err := fingerprintAction(domain.ActionFormulaRetired, map[string]any{
		"formula_id":       id,
		"expected_version": versionFingerprint(input.ExpectedVersion),
	})
	if err != nil {
		return domain.Formula{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}

	var updated domain.Formula
	var replayed bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		replay, err := beginIdempotent(snapshot, input.IdempotencyKey, fingerprint)
		if err != nil {
			return err
		}
		if replay.replayed {
			updated, err = replayFormula(snapshot, replay.entry)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		formula, ok := snapshot.Formulas[id]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "formula was not found")
		}
		if err := s.checkExpected(formula.Version, input.ExpectedVersion); err != nil {
			return err
		}
		for _, run := range snapshot.Runs {
			if run.FormulaID == id && run.State != domain.RunCompleted &&
				run.State != domain.RunAborted {
				return domain.NewError(domain.CodeFormulaInUse,
					"formula is attached to a run that is still open")
			}
		}
		now := s.clock.Now()
		if err := formula.Retire(now); err != nil {
			return err
		}
		snapshot.Formulas[id] = formula
		draft := domain.TrajectoryDraft{
			Action:     domain.ActionFormulaRetired,
			OccurredAt: now,
			Refs:       []domain.TrajectoryRef{domain.NewSubjectRef(domain.EntityFormula, id, formula.Version)},
			Payload: map[string]any{
				"retired_at": utcStamp(*formula.RetiredAt),
			},
		}
		if _, err := appendEntry(snapshot, draft, now, input.IdempotencyKey, fingerprint); err != nil {
			return err
		}
		updated = formula.Clone()
		return nil
	})
	if err != nil {
		return domain.Formula{}, false, err
	}
	return updated, replayed, nil
}

func (s *Service) GetFormula(id string) (domain.Formula, error) {
	var found domain.Formula
	err := s.store.View(func(snapshot domain.Snapshot) error {
		formula, ok := snapshot.Formulas[id]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "formula was not found")
		}
		found = formula.Clone()
		return nil
	})
	if err != nil {
		return domain.Formula{}, err
	}
	return found, nil
}

func (s *Service) ListFormulas() ([]domain.Formula, error) {
	var formulas []domain.Formula
	err := s.store.View(func(snapshot domain.Snapshot) error {
		formulas = make([]domain.Formula, 0, len(snapshot.Formulas))
		for _, formula := range snapshot.Formulas {
			formulas = append(formulas, formula.Clone())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(formulas, func(i, j int) bool {
		if formulas[i].Name == formulas[j].Name {
			return formulas[i].ID < formulas[j].ID
		}
		return formulas[i].Name < formulas[j].Name
	})
	return formulas, nil
}

// versionFingerprint renders an optional expected version for fingerprinting;
// absent and present-zero must hash differently from each other.
func versionFingerprint(value *int64) any {
	if value == nil {
		return map[string]any{"present": false}
	}
	return map[string]any{"present": true, "version": *value}
}

// stepsFingerprint renders the client-controlled step list in its request
// order so identical retries hash identically.
func stepsFingerprint(steps []domain.FormulaStep) []any {
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
