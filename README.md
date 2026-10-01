# Cellar Run Control

Cellar Run Control is a local Go HTTP service for coordinating a small
production cellar. It keeps formula approval, vessel availability, active
fermentation runs, gravity observations, conditioning, completion metrics, and
vessel cleaning in one deterministic process.

In addition to the current aggregate state, the service maintains an
append-only production trajectory ledger. Every state-changing business action
(formula approval, vessel reservation, fermentation start, each observation,
conditioning, completion, vessel cleaning, and cleaning release) appends one
immutable, hash-chained ledger row in the same atomic transaction that writes
the business state. Rows reference the formula, vessel, and run they belong to,
so quality control can replay exactly what happened, in what order, and along
which lineage.

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
internal/domain                entities, validation, transitions, metrics, ledger
internal/httpapi               HTTP routes, JSON decoding, errors, ledger reads
internal/service               use-case orchestration and idempotency
internal/storage               revisioned snapshot persistence and chain checks
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

Production trajectory ledger:

- `GET /v1/trajectory` — paged ledger, newest-independent forward order
- `GET /v1/trajectory/export` — the same rows as newline-delimited JSON
- `GET /v1/trajectory/entries/{entryID}` — one immutable ledger row
- `GET /v1/formulas/{id}/trajectory`
- `GET /v1/vessels/{id}/trajectory`
- `GET /v1/runs/{id}/trajectory`

Ledger rows never change. Each row carries a gapless `sequence`, a stable
`id`, the closed-set `action`, the business `occurred_at`, `refs` (one
`subject` reference with the produced aggregate version plus `lineage`
references to the related formula/vessel/run), an action-specific `payload`,
and the hash link (`prev_hash`, `entry_hash`).

```json
{
  "sequence": 12,
  "id": "ledger_9f3a...",
  "action": "run_completed",
  "occurred_at": "2026-10-01T09:04:00Z",
  "recorded_at": "2026-10-01T09:04:00Z",
  "refs": [
    {"kind": "run", "id": "run_...", "role": "subject", "version": 8},
    {"kind": "vessel", "id": "vessel_...", "role": "lineage"},
    {"kind": "formula", "id": "formula_...", "role": "lineage"}
  ],
  "payload": {"run_state": "completed", "metrics": {"...": "..."}},
  "prev_hash": "…",
  "entry_hash": "…"
}
```

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

Writes also accept an optional `Idempotency-Key` header (8..128 printable
characters). A retried write that reuses the same key with the same parameters
returns the original result, sets `Idempotency-Replayed: true`, and appends no
second ledger row. Reusing a key with different parameters returns
`idempotency_conflict`. Even without a key, one aggregate version can never
produce two ledger rows.

### Reading the ledger

`GET /v1/trajectory` and the entity-scoped trajectory routes accept:

- `limit` — page size (default 50, maximum 200);
- `cursor` — the opaque `next_cursor` token from the previous page;
- `entity=formula|vessel|run&entity_id=...` — lineage filter on
  `/v1/trajectory` (entity routes already scope by path id);
- `since`, `until` — RFC3339 bounds on the business `occurred_at`.

Pages return `{ "entries": [...], "next_cursor": "...", "limit": n }`. The
cursor encodes a sequence position, so paging is stable while new actions are
appended and remains identical after an export or a process restart. The export
endpoint streams every matching row as newline-delimited JSON in the same
sequence order.

## Workflow Checks

Run each production workflow check separately:

```bash
go run ./cmd/fermentctl check formula-approval
go run ./cmd/fermentctl check vessel-run-lifecycle
go run ./cmd/fermentctl check observations-completion
go run ./cmd/fermentctl check trajectory-ledger
```

Each check creates a temporary snapshot directory, starts the real HTTP
handler on a loopback listener, issues JSON requests, verifies success and
failure behavior, and removes its temporary data. The ledger check additionally
verifies hash-chain continuity, lineage reads, cursor paging, the NDJSON export,
idempotent replay (including same-key/different-parameter rejection), the
absence of ledger rows for failed actions, and identical order and hashes after
reopening the store.

The repository also contains Go unit tests (`go test ./...`) covering ledger
sealing and tampering detection, version and idempotency-key dedup, transaction
rollback and persistence failures, startup verification, cursor stability, and
concurrent writes under the race detector.

## Persistence And Recovery

The service stores one complete revisioned snapshot at
`<data-directory>/state.json`, including the business aggregates and the
trajectory ledger with its derived indexes. A write creates a temporary file in
the same directory, encodes the business state and the new ledger rows together,
commits the bytes durably, closes the handle, and renames the result over the
previous snapshot. The in-memory snapshot is replaced only after that operation
succeeds. Consequently a business fact and its ledger row cannot be committed
on only one side: if ledger sealing or persistence fails, the whole write is
discarded.

On startup, an existing snapshot is decoded and normalized. The ledger hash
chain is replayed (gapless sequence, unique ids, every `prev_hash` and
`entry_hash`), and the id/version indexes are rebuilt from the rows themselves;
an unknown or conflicting index, a broken chain, invalid JSON, or a negative
revision stops the process before HTTP serving begins.

## Scope

The service does not control physical equipment, authenticate remote users,
send notifications, expose a browser interface, or replicate data across
processes. It is intended for a trusted local network or an internal adapter.
