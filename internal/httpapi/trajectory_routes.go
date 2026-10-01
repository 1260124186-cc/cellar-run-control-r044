package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

func registerTrajectoryRoutes(mux *http.ServeMux, app *service.Service) {
	mux.HandleFunc("GET /v1/trajectory", func(response http.ResponseWriter, request *http.Request) {
		query, ok := parseTrajectoryQuery(response, request, false)
		if !ok {
			return
		}
		page, err := app.ReadTrajectory(query)
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{
			"entries":     page.Entries,
			"next_cursor": page.NextCursor,
			"limit":       page.Limit,
		})
	})

	mux.HandleFunc("GET /v1/trajectory/export", func(response http.ResponseWriter, request *http.Request) {
		query, ok := parseTrajectoryQuery(response, request, false)
		if !ok {
			return
		}
		response.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		response.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(response)
		err := app.ExportTrajectory(query, func(entry domain.TrajectoryEntry) error {
			return encoder.Encode(entry)
		})
		if err != nil {
			// Headers and body are already streaming, so the failure can only
			// be reported through the connection; nothing else is left to write.
			return
		}
	})

	mux.HandleFunc("GET /v1/trajectory/entries/{entryID}", func(response http.ResponseWriter, request *http.Request) {
		entry, err := app.ReadTrajectoryEntry(request.PathValue("entryID"))
		if err != nil {
			writeDomainError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, entry)
	})

	mux.HandleFunc("GET /v1/formulas/{id}/trajectory", func(response http.ResponseWriter, request *http.Request) {
		writeEntityTrajectory(response, request, app, domain.EntityFormula)
	})
	mux.HandleFunc("GET /v1/vessels/{id}/trajectory", func(response http.ResponseWriter, request *http.Request) {
		writeEntityTrajectory(response, request, app, domain.EntityVessel)
	})
	mux.HandleFunc("GET /v1/runs/{id}/trajectory", func(response http.ResponseWriter, request *http.Request) {
		writeEntityTrajectory(response, request, app, domain.EntityRun)
	})
}

func writeEntityTrajectory(
	response http.ResponseWriter,
	request *http.Request,
	app *service.Service,
	kind domain.EntityKind,
) {
	query, ok := parseTrajectoryQuery(response, request, false)
	if !ok {
		return
	}
	query.Kind = kind
	query.ID = request.PathValue("id")
	query.MustExist = true
	page, err := app.ReadTrajectory(query)
	if err != nil {
		writeDomainError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"entries":     page.Entries,
		"next_cursor": page.NextCursor,
		"limit":       page.Limit,
	})
}

func parseTrajectoryQuery(
	response http.ResponseWriter,
	request *http.Request,
	_ bool,
) (service.TrajectoryQuery, bool) {
	values := request.URL.Query()
	query := service.TrajectoryQuery{
		AfterCursor: values.Get("cursor"),
	}
	switch kind := values.Get("entity"); kind {
	case "":
	case "formula":
		query.Kind = domain.EntityFormula
	case "vessel":
		query.Kind = domain.EntityVessel
	case "run":
		query.Kind = domain.EntityRun
	default:
		writeDomainError(response, domain.FieldError(domain.CodeInvalidInput, "entity",
			"must be one of formula, vessel, run"))
		return service.TrajectoryQuery{}, false
	}
	if query.Kind != "" {
		query.ID = values.Get("entity_id")
		if query.ID == "" {
			writeDomainError(response, domain.FieldError(domain.CodeInvalidInput, "entity_id",
				"is required when filtering by entity"))
			return service.TrajectoryQuery{}, false
		}
	}
	if raw := values.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeDomainError(response, domain.FieldError(domain.CodeInvalidInput, "since",
				"must be an RFC3339 timestamp"))
			return service.TrajectoryQuery{}, false
		}
		query.Since = &parsed
	}
	if raw := values.Get("until"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeDomainError(response, domain.FieldError(domain.CodeInvalidInput, "until",
				"must be an RFC3339 timestamp"))
			return service.TrajectoryQuery{}, false
		}
		query.Until = &parsed
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			writeDomainError(response, domain.FieldError(domain.CodeInvalidInput, "limit",
				"must be a positive integer"))
			return service.TrajectoryQuery{}, false
		}
		query.Limit = limit
	}
	return query, true
}
