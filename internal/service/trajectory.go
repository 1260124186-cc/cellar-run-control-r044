package service

import (
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ledger"
)

// TrajectoryQuery carries a trajectory read request from the transport
// layer into the service.
type TrajectoryQuery struct {
	EntityType string
	EntityID   string
	From       *time.Time
	After      int64
	Limit      int
}

// PrepareTrajectoryFilter validates read parameters and builds the ledger
// filter used by both paged reads and streaming exports, so transport code
// can reject bad parameters before committing response headers.
func (s *Service) PrepareTrajectoryQuery(query TrajectoryQuery) (ledger.Filter, int, error) {
	filter, err := buildTrajectoryFilter(query.EntityType, query.EntityID, query.From)
	if err != nil {
		return ledger.Filter{}, 0, err
	}
	limit := query.Limit
	if limit < 1 {
		limit = ledger.DefaultPageLimit
	}
	if limit > ledger.MaxPageLimit {
		limit = ledger.MaxPageLimit
	}
	return filter, limit, nil
}

// ReadTrajectory returns one stable, ordered page of the production ledger.
func (s *Service) ReadTrajectory(query TrajectoryQuery) (ledger.Page, error) {
	filter, limit, err := s.PrepareTrajectoryQuery(query)
	if err != nil {
		return ledger.Page{}, err
	}
	var page ledger.Page
	err = s.store.View(func(snapshot domain.Snapshot) error {
		page = snapshot.Trajectory.Query(filter, limit, query.After)
		return nil
	})
	if err != nil {
		return ledger.Page{}, err
	}
	return page, nil
}

// ExportTrajectory streams the complete ledger in global sequence order,
// optionally restricted by entity lineage or an occurrence-time cursor.
// The visitor receives independent copies of every entry.
func (s *Service) ExportTrajectory(
	filter ledger.Filter,
	visitor func(ledger.Entry) error,
) error {
	return s.store.View(func(snapshot domain.Snapshot) error {
		return snapshot.Trajectory.Walk(filter, visitor)
	})
}

func buildTrajectoryFilter(entityType, entityID string, from *time.Time) (ledger.Filter, error) {
	filter := ledger.Filter{From: from}
	if entityType == "" && entityID == "" {
		return filter, nil
	}
	if entityType == "" || entityID == "" {
		return ledger.Filter{}, domain.FieldError(domain.CodeInvalidInput,
			"entity_type", "entity_type and entity_id must be provided together")
	}
	parsed := ledger.EntityType(entityType)
	if parsed != ledger.EntityFormula && parsed != ledger.EntityVessel && parsed != ledger.EntityRun {
		return ledger.Filter{}, domain.FieldError(domain.CodeInvalidInput,
			"entity_type", "must be one of formula, vessel, run")
	}
	filter.EntityType = parsed
	filter.EntityID = entityID
	return filter, nil
}

func formulaRef(formula domain.Formula) ledger.Ref {
	return ledger.Ref{Type: ledger.EntityFormula, ID: formula.ID, Code: formula.Name}
}

func vesselRef(vessel domain.Vessel) ledger.Ref {
	return ledger.Ref{Type: ledger.EntityVessel, ID: vessel.ID, Code: vessel.Code}
}

func runRef(run domain.FermentationRun) ledger.Ref {
	return ledger.Ref{Type: ledger.EntityRun, ID: run.ID, Code: run.Code}
}

// appendTrajectory records one entry inside an open snapshot transaction.
// An append failure aborts the whole transaction, so a business state
// change can never commit without its ledger row (and vice versa).
func appendTrajectory(snapshot *domain.Snapshot, entry ledger.Entry) error {
	if _, err := snapshot.Trajectory.Append(entry); err != nil {
		return domain.NewError(domain.CodeStorage, "trajectory append failed: "+err.Error())
	}
	return nil
}
