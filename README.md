# agent-loop

A Go-native runtime for long-running, tool-using, multi-agent workflows.
This repo contains both the design (under [docs/](docs/)) and a working
first-slice implementation that runs on `go test ./...` with no external
services.

## Design docs

- [Requirements](requirement.md)
- [Domain model and state machines](docs/01-domain-model.md)
- [PostgreSQL schema sketch](docs/02-postgres-schema.sql)
- [Go interface contracts](docs/03-go-interfaces.md)
- [Development contracts and test scenarios](docs/04-contracts-and-tests.md)

## What is implemented

The recommended first vertical slice from
[docs/04-contracts-and-tests.md](docs/04-contracts-and-tests.md):

| Capability | Where |
| --- | --- |
| Core domain types, IDs, events, limits, usage | [core/types.go](core/types.go) |
| Event-sourced in-memory store (sessions, runs, tasks, events, approvals, artifacts) with monotonic per-run sequence allocation under concurrency | [store/store.go](store/store.go) |
| PostgreSQL driver (pgx v5, pgxpool, embedded migration, polling Subscribe) | [store/pg/pg.go](store/pg/pg.go) |
| Provider-agnostic model abstraction (`Provider` interface) | [model/model.go](model/model.go) |
| Mock provider for deterministic tests | [model/mock.go](model/mock.go) |
| Ollama provider (`/api/chat`) | [model/ollama.go](model/ollama.go) |
| Native Go tool registry + builtin demo tools (`echo`, `uppercase`, `math.add`) | [tool/tool.go](tool/tool.go) |
| Tool policy (allow / deny / approval) | [policy/policy.go](policy/policy.go) |
| Budget manager (model calls, tool calls, sub-agents, tokens, cost) | [policy/policy.go](policy/policy.go) |
| Loop / stall detector | [policy/loop.go](policy/loop.go) |
| Orchestrator with sequential and parallel sub-agent stages, fan-in policies (`fail_fast`, `wait_all`, `best_effort`), parallel tool calls for read-safe tools, idempotent run start, cancellation | [runtime/orchestrator.go](runtime/orchestrator.go) |
| Strict replay (rebuild terminal state from events) | [runtime/replay.go](runtime/replay.go) |
| REST + SSE API with `Last-Event-ID` resume and heartbeats | [api/server.go](api/server.go) |
| Embedded responsive debug console for session resume, chat-style run testing, setup inspection, REST checkpoints, request logs, and HTTP SSE | [api/web/index.html](api/web/index.html) |
| HTTP server example | [cmd/agent-loop/main.go](cmd/agent-loop/main.go) |
| Programmatic example: planner -> parallel researchers -> synthesizer + replay | [cmd/example-workflow/main.go](cmd/example-workflow/main.go) |

The MCP client, snapshot/compaction, and pgvector memory are still
doc-only. Both the in-memory store and the PostgreSQL driver implement
the aggregate `store.Store` interface, so `runtime` and `api` are
driver-agnostic.

## Run it

Default test suite (no external services, runs in CI):

```sh
go test -race -count=1 ./...
```

Programmatic workflow demo:

```sh
go run ./cmd/example-workflow
```

HTTP server with mock provider:

```sh
go run ./cmd/agent-loop
# open http://localhost:8080/ for the debug console
# then in another shell
curl -X POST http://localhost:8080/v1/sessions -d '{"title":"demo"}'
# -> {"session_id":"…"}
curl -X POST http://localhost:8080/v1/runs \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"<id>","workflow":{"name":"wf","stages":[{"name":"reply","kind":"sequential","agents":[{"name":"echo","prompt":"Say hi"}]}]}}'
# subscribe to events
curl -N http://localhost:8080/v1/runs/<run_id>/stream
```

The debug console is served by the same Go binary and has no separate
frontend build step. It can create or resume sessions, send human
messages, start agent runs, inspect current agent and tool setup, poll
run state, list run events, cancel runs, resolve approval IDs, call
`/healthz`, and subscribe to run events through HTTP SSE with
`Last-Event-ID` resume. The console also includes REST/SSE checkpoint
coverage and an advanced log tab showing raw request headers, request
bodies, responses, and stream activity.

Debug-only setup endpoints are available for local inspection:

```sh
curl http://localhost:8080/v1/debug/agents
curl http://localhost:8080/v1/debug/tools
```

Optional Ollama integration test (kept behind a build tag so CI doesn't
need a model server):

```sh
OLLAMA_MODEL=gemma4:latest go test -tags=ollama -count=1 ./model/...
```

PostgreSQL integration tests (behind the `pg` build tag):

```sh
docker run -d --rm --name agentloop-pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=agentloop \
  -p 5432:5432 postgres:18-alpine

AGENT_LOOP_PG_DSN='postgres://postgres:postgres@localhost:5432/agentloop?sslmode=disable' \
  go test -tags=pg -race -count=1 ./store/pg/...
```

The driver embeds [store/pg/schema.sql](store/pg/schema.sql) and applies
it on `EnsureSchema(ctx)`. Without `AGENT_LOOP_PG_DSN` set the tests
skip cleanly.

Docker Compose REST integration stack:

```sh
# Host ports are prefixed with 4 to avoid common local conflicts:
# API http://localhost:48080, Postgres localhost:45432, Ollama localhost:41434
OLLAMA_MODEL=gemma4:latest \
   docker compose -f docker-compose.test.yml up --build \
   --abort-on-container-exit --exit-code-from rest-test rest-test

docker compose -f docker-compose.test.yml down
```

If `docker compose ps` does not show `api` immediately, check
`docker compose -f docker-compose.test.yml ps -a`: the API intentionally
stays in `Created` until `ollama-pull` exits successfully. Pulling
`gemma4:latest` can download a multi-GB model layer on first run.

The compose stack starts PostgreSQL 18, `ollama/ollama:latest` CPU mode,
pulls the configured model, runs the API against Postgres and Ollama, and
then executes the REST integration test with the `rest` build tag. The API
server also understands these local testing environment variables outside
Compose:

```sh
AGENT_LOOP_PG_DSN='postgres://postgres:postgres@localhost:45432/agentloop?sslmode=disable' \
OLLAMA_BASE_URL='http://localhost:41434' \
OLLAMA_MODEL='gemma4:latest' \
OLLAMA_TIMEOUT_SECONDS=900 \
go run ./cmd/agent-loop -addr :8080

AGENT_LOOP_REST_URL='http://localhost:8080' \
AGENT_LOOP_REST_TIMEOUT=900 \
go test -tags=rest -count=1 ./api
```

## Continuous integration

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs
`go vet`, `go build`, `go test -race -count=1 ./...`, and the example
workflow on every push and PR.

## Gaps closed vs the original docs

While implementing, the following gaps in `requirement.md` and `docs/`
were resolved in code:

1. **Concrete REST contract** — handlers and request/response shapes in
   [api/server.go](api/server.go) materialize the OpenAPI surface that
   doc 4 only described.
2. **CI-friendly persistence** — an in-memory `EventStore` /
   `RunStore` / `TaskStore` / `ApprovalStore` / `ArtifactStore` that
   satisfies the same interfaces a Postgres driver would. Tests run
   without external services.
3. **Per-run sequence allocator** — sequence numbers are assigned
   atomically inside `Append`, so concurrent goroutines produce a
   strict total order. Verified by `TestEventSequenceIsMonotonicUnderConcurrency`.
4. **Loop / stall detector algorithm** — a simple sliding counter on
   `(tool, hash(input))` per run, configurable threshold. Verified by
   `TestLoopDetectorTripsOnRepeats`.
5. **Replay scope** — strict replay (rebuild from events, verify
   terminal state) is implemented for v1; provider-replay and tool-replay
   remain doc-only.
6. **Concurrent tool group ordering** — parallel-safe tool calls are
   dispatched concurrently but ordered by ordinal before reply messages
   are appended, so persisted history is deterministic.
7. **Single-tenant ergonomics** — `core.DefaultTenant` and
   `core.DefaultPrincipal` let single-tenant deployments ignore tenant
   headers; the data model still carries tenant IDs everywhere.
