# Cellar Run Control

Cellar Run Control is a local Go HTTP service for coordinating a small
production cellar. It keeps formula approval, vessel availability, active
fermentation runs, gravity observations, conditioning, completion metrics, and
vessel cleaning in one deterministic process.

Every key production action is also recorded in an append-only production
trajectory ledger so quality control can reconstruct, per batch, the exact
order of formula release, reservation and transfer, observations, completion,
and vessel cleaning. The ledger and the business state commit in one atomic
snapshot write.

## Requirements

- Go 1.23 or later
- A writable local data directory
- No database, network service, or third-party module

## Build And Run

Build every package:

```bash
go build ./...
```

Start the service:

```bash
go run ./cmd/fermentctl serve
```

The default address is `127.0.0.1:8088`. The default snapshot directory is
`./data`.

Environment variables:

- `CELLAR_CONTROL_ADDR`: HTTP listen address.
- `CELLAR_CONTROL_DATA_DIR`: directory containing `state.json`.

Example:

```bash
CELLAR_CONTROL_ADDR=127.0.0.1:9099 \
CELLAR_CONTROL_DATA_DIR=/tmp/cellar-control \
go run ./cmd/fermentctl serve
```

## Directory Structure

```text
cmd/fermentctl                 service and workflow-check executable
internal/clock                 time boundary
internal/config                environment configuration
internal/domain                entities, validation, transitions, metrics
internal/httpapi               HTTP routes, JSON decoding, errors
internal/ledger                append-only production trajectory kernel
internal/service               use-case orchestration
internal/storage               revisioned snapshot persistence
internal/workflowcheck         real HTTP production checks
```

## Public API

Health:

- `GET /healthz`

Formula workflow:

- `POST /v1/formulas`
- `GET /v1/formulas`
- `GET /v1/formulas/{id}`
- `PUT /v1/formulas/{id}/steps`
- `POST /v1/formulas/{id}/approve`
- `POST /v1/formulas/{id}/retire`

Vessel workflow:

- `POST /v1/vessels`
- `GET /v1/vessels`
- `GET /v1/vessels/{id}`
- `POST /v1/vessels/{id}/finish-cleaning`
- `POST /v1/vessels/{id}/retire`

Run workflow:

- `POST /v1/runs`
- `GET /v1/runs`
- `GET /v1/runs/{id}`
- `POST /v1/runs/{id}/reserve`
- `POST /v1/runs/{id}/start`
- `POST /v1/runs/{id}/observations`
- `POST /v1/runs/{id}/conditioning`
- `POST /v1/runs/{id}/complete`
- `POST /v1/runs/{id}/abort`

Production trajectory (append-only ledger):

- `GET /v1/trajectory` — paged read in global sequence order
- `GET /v1/trajectory/export` — full NDJSON export in global sequence order

Trajectory query parameters (all optional):

- `entity_type` + `entity_id` (must be supplied together): return every
  entry touching the entity either as the subject or through lineage, so a
  batch is traceable from its formula, its vessel, or its run.
- `from`: RFC3339 occurrence-time cursor; keeps entries at or after the
  instant while sequence order remains authoritative.
- `limit` (paged read): 1 to 500, default 100.
- `cursor`: opaque paging cursor returned as `next_cursor`.

Writes accept an optional `X-Expected-Version` header. Errors use a stable
shape:

```json
{
  "error": {
    "code": "revision_conflict",
    "detail": "the aggregate version does not match X-Expected-Version",
    "field": ""
  }
}
```

## Workflow Checks

Run each production workflow check separately:

```bash
go run ./cmd/fermentctl check formula-approval
go run ./cmd/fermentctl check vessel-run-lifecycle
go run ./cmd/fermentctl check observations-completion
go run ./cmd/fermentctl check production-trajectory
```

Each check creates a temporary snapshot directory, starts the real HTTP
handler on a loopback listener, issues JSON requests, verifies success and
failure behavior, and removes its temporary data. The trajectory check also
restarts the process against the same data directory to prove ledger order
is identical after recovery.

## Production Trajectory Ledger

The ledger is append-only. Each committed business action appends exactly one
immutable entry carrying:

- a gapless global `sequence` assigned in commit order;
- a stable `action` (for example `formula.approved`, `vessel.reserved`,
  `run.observation_added`, `run.completed`, `vessel.cleaning_finished`);
- an `occurred_at` business timestamp;
- the `subject` entity (formula, vessel, or run) and the subject
  `entity_version` produced by the action;
- `from_state` / `to_state` for lifecycle transitions;
- a `lineage` list linking the other formula/vessel/run entities involved, so
  the same batch is readable from any side of the bloodline;
- an action-specific `detail` object (gravity, metrics, counts).

Ordering and paging guarantees:

- Entries are always read and exported in global sequence order. Pagination
  uses an opaque `cursor` over sequence positions, so equal timestamps never
  reorder or drop rows and restart does not change order.
- `(subject type, subject id, entity version)` is the idempotency key.
  Re-processing the same version returns the existing entry and never creates
  a second row; a different payload for the same key is rejected.
- The business state and the ledger commit in the same atomic snapshot. If a
  trajectory append fails, the surrounding business transaction is not
  persisted, so no fact can be true on only one side.
- On startup the persisted entries are validated: sequences must be gapless
  from 1 and idempotency keys must be unique. A corrupt ledger stops the
  process before HTTP serving begins.

## Persistence And Recovery

The service stores a complete revisioned snapshot at
`<data-directory>/state.json`, including the trajectory entries. A write
creates a temporary file in the same directory, commits the encoded bytes
durably, closes the handle, and renames the result over the previous snapshot.
The in-memory snapshot is replaced only after that operation succeeds.

On startup, an existing snapshot is decoded and normalized. Unknown or
conflicting map indexes are not silently invented; invalid JSON, a negative
revision, or a non-gapless or duplicate-key trajectory stops the process
before HTTP serving begins.

## Repair-Ready Boundaries

The baseline intentionally keeps several independent behavior surfaces visible
for later engineering evaluation:

- formula name normalization and duplicate detection;
- target gravity and process-step range validation;
- aggregate version conflict checks;
- atomic vessel reservation and release;
- run state transition guards;
- observation sequence and time ordering;
- gravity movement and range checks;
- metric calculation and rounding;
- durable snapshot replacement and startup normalization.

These are production mechanisms, not injected defects. A later task can add
automated tests or request a focused repair without changing the public API.

## Test Boundary

The trajectory work ships with focused automated coverage:

- `internal/ledger` unit tests: gapless sequencing, idempotent append and
  payload conflict, lineage and time-cursor reads, stable cursor paging, JSON
  round trip that rebuilds indexes, and corruption rejection.
- `internal/storage` tests: business state rollback when a trajectory append
  fails, sequence continuation and identical order across a restart, and
  in-transaction idempotency.
- The `production-trajectory` production check exercises the real HTTP API
  end to end, including full NDJSON export and a live process restart.

Systematic coverage of the remaining validation limits, state transitions,
concurrency, and numeric edge cases is still owned by the later test task.

## Scope

The service does not control physical equipment, authenticate remote users,
send notifications, expose a browser interface, or replicate data across
processes. It is intended for a trusted local network or an internal adapter.
