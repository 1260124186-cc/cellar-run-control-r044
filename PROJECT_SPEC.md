# Cellar Run Control Specification

## Goal

Cellar Run Control is a local Go HTTP service for a small production cellar.
It coordinates approved formulas, vessel reservation, active fermentation
runs, gravity observations, conditioning, completion, and vessel release.

Every business state change is also recorded in an append-only production
trajectory ledger. The ledger, not the current aggregate fields alone, is the
quality-control record of what happened: which observation, vessel change, or
completion occurred first, along which formula -> run -> vessel lineage.

The project is a healthy initialization baseline extended with the trajectory
ledger, focused unit tests, and a fourth bounded workflow check. It contains
no intentional defects.

## Users

- Production planner: creates formulas, updates process steps, and approves a
  formula before it can be used.
- Cellar operator: registers vessels, creates runs, reserves a vessel, and
  starts fermentation.
- Fermentation observer: appends timestamped gravity observations and moves a
  run through conditioning and completion.
- Integration client: calls the stable JSON API from a local production script.

## Entities

### Formula

A formula contains an identifier, normalized name, style label, target original
gravity, target final gravity, maximum fermentation temperature, minimum and
maximum planned days, ordered process steps, lifecycle state, version, and
timestamps.

States are `draft`, `approved`, and `retired`.

- Names are unique after case folding and whitespace normalization.
- Target original gravity is in the inclusive range 1.030 to 1.120.
- Target final gravity is positive, lower than target original gravity, and
  differs from it by at least 0.005.
- Maximum fermentation temperature is in the inclusive range 5 to 40 Celsius.
- Planned duration is 1 to 120 days and the minimum cannot exceed the maximum.
- A formula has 1 to 24 process steps.
- Step sequence values must be contiguous and start at 1.
- Step temperatures are in the inclusive range 5 to 90 Celsius.
- Step hold times are 1 to 1440 minutes.
- `draft -> approved` requires a complete valid step list.
- An approved formula is immutable.
- `approved -> retired` is allowed only when no run currently uses it.

### Vessel

A vessel contains an identifier, unique display code, capacity in liters,
lifecycle state, active run identifier, version, and timestamps.

States are `available`, `reserved`, `in_use`, `cleaning`, and `retired`.

- Capacity is in the inclusive range 10 to 5000 liters.
- Only an available vessel can be reserved.
- A reserved or in-use vessel must identify exactly one active run.
- Completion moves an in-use vessel to cleaning.
- Cleaning can be finished only when the associated run is complete or
  aborted.
- Finishing cleaning releases the vessel to available.
- A vessel with an active run cannot be retired or directly reserved.

### FermentationRun

A run contains an identifier, unique run code, an approved formula identifier,
an optional vessel identifier, planned volume, notes, lifecycle state,
timetable, ordered observations, final metrics, version, and timestamps.

States are `planned`, `reserved`, `fermenting`, `conditioning`, `completed`,
and `aborted`.

- Planned volume is greater than zero and cannot exceed vessel capacity.
- Only an approved formula may be attached to a run.
- `planned -> reserved` atomically reserves an available vessel.
- `reserved -> fermenting` requires that the vessel still belongs to the run.
- `fermenting -> conditioning` requires at least two observations.
- `conditioning -> completed` requires at least three observations.
- Completion computes apparent attenuation and estimated alcohol by volume from
  the formula target original gravity and the last observation.
- Completion moves the vessel to cleaning without losing its run association.
- A non-completed run can be aborted. Aborting before fermentation releases an
  available vessel; aborting after fermentation schedules the vessel for
  cleaning.
- Completed and aborted runs are immutable.
- A run can carry at most 500 observations.

### GravityObservation

An observation contains a run identifier, contiguous sequence number, observed
time, gravity, temperature, optional note, and receipt time.

- Gravity is in the inclusive range 0.980 to 1.200.
- Temperature is in the inclusive range -5 to 60 Celsius.
- Observed time begins at or after the run start.
- Observation times strictly increase within a run.
- Gravity must not rise more than 0.010 above the immediately preceding value.
- Observation sequences are contiguous and start at 1.

### ProductionTrajectory

The trajectory is the append-only ledger of business actions. It is a gapless,
hash-chained sequence of immutable entries stored alongside the aggregates.

An entry contains a global contiguous `sequence` starting at 1, a unique `id`,
a closed-set `action`, a business `occurred_at`, a persistence `recorded_at`,
a reference list, an action-specific `payload`, `prev_hash`, and `entry_hash`.

- References identify exactly one `subject` (the aggregate the action acts on,
  with the version the action produced) and any number of `lineage` references
  to the related formula, vessel, and run.
- `entry_hash` signs the canonical JSON of the entry fields including
  `prev_hash`; key order is canonicalized so hashes survive a JSON round trip.
- The first entry has an empty `prev_hash`; every later entry signs the
  previous entry's hash. Deleting, reordering, or altering any row breaks the
  chain and is rejected at startup.
- The action set is closed and covers formula create/steps/approve/retire,
  vessel create/reserve/cleaning-start/cleaning-finish/retire/reservation-
  release, and run create/start/observation/conditioning/complete/abort.
- One subject aggregate version maps to exactly one ledger entry. The version
  index is derived from the ledger and rebuilt on load.
- A business mutation and its ledger entry are appended inside one atomic
  snapshot transaction. If validation or persistence fails, neither the
  aggregate change nor the ledger row is visible.
- Reads traverse the stored slice, which is the sequence order. Paged reads
  use opaque sequence cursors, and time-window reads filter on `occurred_at`.
  Both are stable while rows are appended, under export, and across restart.
- Clients may send an `Idempotency-Key` on writes. The key plus a fingerprint
  of request-controlled parameters is stored in the same transaction. The same
  key with the same parameters replays the original result without a new entry;
  the same key with different parameters returns `idempotency_conflict`.

## Connected Workflows

### 1. Formula Approval

Public entry points are:

- `POST /v1/formulas`
- `PUT /v1/formulas/{id}/steps`
- `POST /v1/formulas/{id}/approve`
- `GET /v1/formulas/{id}`

The client creates a draft, sets its process step list atomically, approves it,
and reads the approved record. Invalid ranges, duplicate names, empty steps,
and repeated approval return stable error codes without partial writes.

This path crosses HTTP protocol handling, application orchestration, domain
rules, and snapshot storage.

### 2. Vessel Run Lifecycle

Public entry points are:

- `POST /v1/vessels`
- `POST /v1/runs`
- `POST /v1/runs/{id}/reserve`
- `POST /v1/runs/{id}/start`
- `GET /v1/runs/{id}`

The client registers a vessel, creates a planned run against an approved
formula, reserves the vessel, starts fermentation, and reads the run. Capacity
conflicts, an unavailable vessel, duplicate run codes, and stale versions
produce stable failures without changing vessel or run state.

This path crosses HTTP protocol handling, application orchestration, domain
rules, storage coordination, and resource lifecycle.

### 3. Observations And Completion

Public entry points are:

- `POST /v1/runs/{id}/observations`
- `POST /v1/runs/{id}/conditioning`
- `POST /v1/runs/{id}/complete`
- `GET /v1/runs/{id}`

The client appends three ordered observations, starts conditioning, completes
the run, and reads the calculated result. Non-monotonic times, rising gravity,
out-of-range values, excessive sequences, incomplete data, and repeated
transitions return stable failures without changing persisted state.

This path crosses HTTP protocol handling, application orchestration, numeric
domain rules, and snapshot storage.

### 4. Production Trajectory Replay

Public entry points are:

- `GET /v1/trajectory`
- `GET /v1/trajectory/export`
- `GET /v1/trajectory/entries/{entryID}`
- `GET /v1/runs/{id}/trajectory`
- `GET /v1/vessels/{id}/trajectory`
- `GET /v1/formulas/{id}/trajectory`

Quality control drives a batch through approval, reservation, fermentation,
observations, conditioning, completion, and vessel cleaning. Every accepted
action appends one hash-chained ledger row in the same transaction as the
aggregate change; rejected actions append nothing. The ledger is then read by
global cursor pages, by entity lineage, and by occurrence-time window, and
exported as NDJSON. Cursors and row order stay identical while new rows append
and after reopening the data directory. Repeating a write with the same
`Idempotency-Key` and parameters returns the original result with no second row.

This path crosses HTTP protocol handling, use-case orchestration, domain ledger
rules, atomic snapshot storage, and recovery.

## Modules

- `cmd/fermentctl`: executable entry point for the HTTP service and bounded
  workflow checks.
- `internal/config`: environment and command-line configuration.
- `internal/clock`: deterministic clock boundary for production and checks.
- `internal/domain`: entities, validation, state transitions, metrics, and the
  trajectory ledger types, sealing, hashing, verification, and indexes.
- `internal/service`: use-case orchestration, revision coordination, ledger
  append coordination, idempotency fingerprints, and ledger reads/export.
- `internal/storage`: snapshot repository, atomic replacement, locking, and
  startup ledger verification.
- `internal/httpapi`: routing, strict JSON decoding, error mapping, idempotency
  headers, ledger pagination, entity timelines, and NDJSON export.
- `internal/workflowcheck`: real HTTP workflow checks used by the executable.

The dependency direction is:

`cmd -> workflowcheck/httpapi -> service -> domain`

`service -> storage`

`storage -> domain`

The domain package does not import outer packages.

## API Behavior

All business routes are JSON under `/v1`. A successful write returns the
updated aggregate and its version; a replayed idempotent write also sets
`Idempotency-Replayed: true`. A failed write returns:

```json
{
  "error": {
    "code": "stable_machine_code",
    "detail": "human-readable explanation"
  }
}
```

Write requests may include `X-Expected-Version`. When present, the service
rejects a stale write instead of overwriting newer state. Write requests may
also include `Idempotency-Key` (8..128 printable characters); the same key and
parameters replay the original result, the same key with different parameters
returns `idempotency_conflict`.

Ledger reads are:

- `GET /v1/trajectory`
- `GET /v1/trajectory/export`
- `GET /v1/trajectory/entries/{entryID}`
- `GET /v1/formulas/{id}/trajectory`
- `GET /v1/vessels/{id}/trajectory`
- `GET /v1/runs/{id}/trajectory`

The paged routes accept `limit`, `cursor`, and, on the global route,
`entity`/`entity_id`; all ledger routes accept RFC3339 `since`/`until`. Pages
return `entries`, opaque `next_cursor`, and `limit`. The export route returns
newline-delimited JSON in the same sequence order.

`GET /healthz` returns process health, the current snapshot revision, and the
latest trajectory sequence.

## Persistence And Concurrency

The configured data directory contains `state.json`, which holds the
aggregates, lookup indexes, the trajectory entries, and the idempotency index.
Writes are serialized by an in-process mutex. The complete state (business
changes and ledger rows together) is encoded, committed to durable storage,
and then atomically renamed over the target. An update is not published in
memory unless persistence succeeds, so a business fact and its ledger entry
cannot exist on only one side.

On startup the trajectory hash chain is replayed and the entry id and aggregate
version indexes are rebuilt from the ledger rows. A gap, duplicate entry id,
duplicate aggregate version, broken hash link, or invalid idempotency pointer
stops the process before HTTP serving begins.

Reads use copied values and never expose repository-owned maps or slices. The
default address is `127.0.0.1:8088`, the request body limit is 1 MiB, and all
workflow checks use temporary directories.

## Validation Plan

The repository ships focused Go unit tests under `internal/domain`,
`internal/service`, and `internal/storage`, covering ledger sealing and hash
verification, tamper detection, version dedup, idempotent replay, transaction
rollback, persistence failures, startup verification, cursor stability, entity
and time-window reads, and concurrent writes under the race detector.

The current build command is `go build ./...`; the current test command is
`go test ./...`. Four bounded production checks start the real HTTP handler,
issue real JSON requests, check success and failure behavior, and exit without
external services:

- `go run ./cmd/fermentctl check formula-approval`
- `go run ./cmd/fermentctl check vessel-run-lifecycle`
- `go run ./cmd/fermentctl check observations-completion`
- `go run ./cmd/fermentctl check trajectory-ledger`

## Scope Limits

This service does not control physical equipment, connect to a remote identity
provider, send notifications, expose a browser interface, replicate data, or
replace laboratory measurements. The API is intended for a trusted local
network or an internal adapter.
