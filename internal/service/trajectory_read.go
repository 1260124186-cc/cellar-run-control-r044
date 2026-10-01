package service

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

const (
	defaultTrajectoryLimit = 50
	maxTrajectoryLimit     = 200
)

// TrajectoryQuery selects ledger rows for paged reads and exports.
//
// The ledger has one total order (sequence). Kind/ID restrict rows to one
// entity through subject or lineage references; Since/Until restrict by the
// business occurrence time. AfterCursor is the opaque token handed back by a
// previous page, so paging never depends on wall-clock time.
type TrajectoryQuery struct {
	Kind        domain.EntityKind
	ID          string
	Since       *time.Time
	Until       *time.Time
	AfterCursor string
	Limit       int
	// MustExist makes a missing filtered entity a not-found error instead of
	// an empty page.
	MustExist bool
}

// TrajectoryPage is one stable slice of the ledger plus the token for the
// next slice. NextCursor is empty when no rows remain.
type TrajectoryPage struct {
	Entries    []domain.TrajectoryEntry `json:"entries"`
	NextCursor string                   `json:"next_cursor,omitempty"`
	Limit      int                      `json:"limit"`
}

// EncodeTrajectoryCursor renders a sequence position as an opaque page token.
func EncodeTrajectoryCursor(sequence int64) string {
	if sequence < 1 {
		return ""
	}
	payload := fmt.Sprintf("seq:%d", sequence)
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// DecodeTrajectoryCursor parses a page token. An empty token means "from the
// beginning".
func DecodeTrajectoryCursor(cursor string) (int64, error) {
	if strings.TrimSpace(cursor) == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return 0, domain.FieldError(domain.CodeInvalidInput, "cursor",
			"must be a trajectory page cursor returned by a previous read")
	}
	text := string(raw)
	if !strings.HasPrefix(text, "seq:") {
		return 0, domain.FieldError(domain.CodeInvalidInput, "cursor",
			"is not a recognized trajectory cursor")
	}
	sequence, err := strconv.ParseInt(strings.TrimPrefix(text, "seq:"), 10, 64)
	if err != nil || sequence < 0 {
		return 0, domain.FieldError(domain.CodeInvalidInput, "cursor",
			"is not a recognized trajectory cursor")
	}
	return sequence, nil
}

func (s *Service) TrajectorySize() int64 {
	return s.store.TrajectorySize()
}

// ReadTrajectoryEntry fetches a single immutable ledger row by entry id.
func (s *Service) ReadTrajectoryEntry(entryID string) (domain.TrajectoryEntry, error) {
	var found domain.TrajectoryEntry
	err := s.store.View(func(snapshot domain.Snapshot) error {
		sequence, ok := snapshot.TrajectoryIDs[entryID]
		if !ok {
			return domain.NewError(domain.CodeTrajectoryNotFound, "trajectory entry was not found")
		}
		found = snapshot.Trajectory[sequence-1].Clone()
		return nil
	})
	if err != nil {
		return domain.TrajectoryEntry{}, err
	}
	return found, nil
}

// selectTrajectory applies the query predicates against one committed
// snapshot. The slice is iterated in stored order, which is by construction
// the sequence order, so every reader observes the same stable ordering.
func selectTrajectory(snapshot domain.Snapshot, query TrajectoryQuery) ([]domain.TrajectoryEntry, error) {
	if query.Kind != "" {
		switch query.Kind {
		case domain.EntityFormula, domain.EntityVessel, domain.EntityRun:
		default:
			return nil, domain.FieldError(domain.CodeInvalidInput, "entity",
				"must be one of formula, vessel, run")
		}
		if query.ID == "" {
			return nil, domain.FieldError(domain.CodeInvalidInput, "entity_id",
				"is required when filtering by entity")
		}
		if query.MustExist {
			var exists bool
			switch query.Kind {
			case domain.EntityFormula:
				_, exists = snapshot.Formulas[query.ID]
			case domain.EntityVessel:
				_, exists = snapshot.Vessels[query.ID]
			case domain.EntityRun:
				_, exists = snapshot.Runs[query.ID]
			}
			if !exists {
				switch query.Kind {
				case domain.EntityFormula:
					return nil, domain.NewError(domain.CodeFormulaNotFound, "formula was not found")
				case domain.EntityVessel:
					return nil, domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
				case domain.EntityRun:
					return nil, domain.NewError(domain.CodeRunNotFound, "run was not found")
				}
			}
		}
	}
	if query.Since != nil && query.Until != nil && query.Until.Before(*query.Since) {
		return nil, domain.FieldError(domain.CodeInvalidInput, "until",
			"must not be earlier than since")
	}
	matched := make([]domain.TrajectoryEntry, 0)
	for _, entry := range snapshot.Trajectory {
		if query.Kind != "" && !entry.Touches(query.Kind, query.ID) {
			continue
		}
		occurred := entry.OccurredAt.UTC()
		if query.Since != nil && occurred.Before(query.Since.UTC()) {
			continue
		}
		if query.Until != nil && occurred.After(query.Until.UTC()) {
			continue
		}
		matched = append(matched, entry.Clone())
	}
	return matched, nil
}

// ReadTrajectory returns one page of ledger rows.
func (s *Service) ReadTrajectory(query TrajectoryQuery) (TrajectoryPage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = defaultTrajectoryLimit
	}
	if limit > maxTrajectoryLimit {
		limit = maxTrajectoryLimit
	}
	after, err := DecodeTrajectoryCursor(query.AfterCursor)
	if err != nil {
		return TrajectoryPage{}, err
	}

	var page TrajectoryPage
	err = s.store.View(func(snapshot domain.Snapshot) error {
		matched, err := selectTrajectory(snapshot, query)
		if err != nil {
			return err
		}
		rows := make([]domain.TrajectoryEntry, 0, limit)
		var lastSequence int64
		for _, entry := range matched {
			if entry.Sequence <= after {
				continue
			}
			if len(rows) >= limit {
				break
			}
			rows = append(rows, entry)
			lastSequence = entry.Sequence
		}
		page.Entries = rows
		page.Limit = limit
		// Only emit a next cursor when another matching row exists beyond
		// this page, so callers do not chase an empty trailing request.
		if len(rows) == limit {
			for _, entry := range matched {
				if entry.Sequence > lastSequence {
					page.NextCursor = EncodeTrajectoryCursor(lastSequence)
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return TrajectoryPage{}, err
	}
	return page, nil
}

// ExportTrajectory streams every matching row in stored sequence order. The
// visitor receives copies; it never owns repository memory.
func (s *Service) ExportTrajectory(query TrajectoryQuery, visitor func(domain.TrajectoryEntry) error) error {
	return s.store.View(func(snapshot domain.Snapshot) error {
		matched, err := selectTrajectory(snapshot, query)
		if err != nil {
			return err
		}
		for _, entry := range matched {
			if err := visitor(entry.Clone()); err != nil {
				return err
			}
		}
		return nil
	})
}
