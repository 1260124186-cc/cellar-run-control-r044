package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ledger"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

type trajectoryResponse struct {
	Entries    []ledger.Entry `json:"entries"`
	NextCursor string         `json:"next_cursor,omitempty"`
	HasMore    bool           `json:"has_more"`
	Limit      int            `json:"limit"`
}

func registerTrajectoryRoutes(mux *http.ServeMux, app *service.Service) {
	mux.HandleFunc("GET /v1/trajectory", func(response http.ResponseWriter, request *http.Request) {
		query, err := parseTrajectoryQuery(request)
		if !handleDecode(response, err) {
			return
		}
		page, err := app.ReadTrajectory(query)
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, trajectoryResponse{
			Entries:    page.Entries,
			NextCursor: page.NextCursor,
			HasMore:    page.HasMore,
			Limit:      page.Limit,
		})
	})

	mux.HandleFunc("GET /v1/trajectory/export", func(response http.ResponseWriter, request *http.Request) {
		values := request.URL.Query()
		var from *time.Time
		if raw := values.Get("from"); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeDomainError(response, domain.FieldError(domain.CodeInvalidInput,
					"from", "must be an RFC3339 timestamp"))
				return
			}
			parsed = parsed.UTC()
			from = &parsed
		}
		filter, _, err := app.PrepareTrajectoryQuery(service.TrajectoryQuery{
			EntityType: values.Get("entity_type"),
			EntityID:   values.Get("entity_id"),
			From:       from,
		})
		if err != nil {
			writeDomainError(response, err)
			return
		}
		flusher, _ := response.(http.Flusher)
		response.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		response.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(response)
		err = app.ExportTrajectory(filter, func(entry ledger.Entry) error {
			if err := encoder.Encode(entry); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		})
		if err != nil {
			// Headers are already committed; append a trailing NDJSON error
			// line a client can detect after a broken stream.
			_ = encoder.Encode(errorResponse{Error: errorBody{
				Code:   domain.CodeStorage,
				Detail: err.Error(),
			}})
			return
		}
	})
}

func parseTrajectoryQuery(request *http.Request) (service.TrajectoryQuery, error) {
	values := request.URL.Query()
	query := service.TrajectoryQuery{
		EntityType: values.Get("entity_type"),
		EntityID:   values.Get("entity_id"),
		Limit:      ledger.DefaultPageLimit,
	}
	if raw := values.Get("limit"); raw != "" {
		parsed, err := parsePositiveInt(raw, ledger.MaxPageLimit)
		if err != nil {
			return service.TrajectoryQuery{}, domain.FieldError(domain.CodeInvalidInput,
				"limit", "must be an integer between 1 and 500")
		}
		query.Limit = parsed
	}
	if raw := values.Get("cursor"); raw != "" {
		sequence, err := ledger.DecodeCursor(raw)
		if err != nil {
			return service.TrajectoryQuery{}, domain.FieldError(domain.CodeInvalidInput,
				"cursor", err.Error())
		}
		query.After = sequence
	}
	if raw := values.Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return service.TrajectoryQuery{}, domain.FieldError(domain.CodeInvalidInput,
				"from", "must be an RFC3339 timestamp")
		}
		parsed = parsed.UTC()
		query.From = &parsed
	}
	return query, nil
}

func parsePositiveInt(raw string, maximum int) (int, error) {
	value := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, errInvalidQueryValue
		}
		value = value*10 + int(r-'0')
		if value > maximum {
			return 0, errInvalidQueryValue
		}
	}
	if value < 1 {
		return 0, errInvalidQueryValue
	}
	return value, nil
}

var errInvalidQueryValue = domain.NewError(domain.CodeInvalidInput, "invalid query value")
