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
| Core domain types, IDs, events, limits, usage, tool definitions | [core/types.go](core/types.go), [core/tool.go](core/tool.go) |
| Event-sourced in-memory store (sessions, runs, tasks, events, approvals, artifacts) with monotonic per-run sequence allocation under concurrency | [store/store.go](store/store.go) |
| PostgreSQL driver (pgx v5, pgxpool, versioned embedded migrations, polling Subscribe) | [store/pg/pg.go](store/pg/pg.go) |
| Provider-agnostic model abstraction (`Provider` interface) | [model/model.go](model/model.go) |
| Mock provider for deterministic tests | [model/mock.go](model/mock.go) |
| Fixture recorder and request-matching replayer for real-model regression tests | [model/fixture.go](model/fixture.go) |
| Ollama provider (`/api/chat`) | [model/ollama/ollama.go](model/ollama/ollama.go) |
| Native Go tool registry + builtin demo tools (`echo`, `uppercase`, `math.add`) | [tool/tool.go](tool/tool.go) |
| Tool policy (allow / deny / approval) | [policy/policy.go](policy/policy.go) |
| Budget manager (model calls, tool calls, sub-agents, tokens, cost) | [policy/policy.go](policy/policy.go) |
| Prometheus metrics at `/metrics` (runs, model calls, tokens, cost, tools, queue depth, HTTP latency) | [observe/metrics.go](observe/metrics.go) |
| Loop / stall detector; idle-wait diagnostics (`idle_wait` events) | [policy/loop.go](policy/loop.go), [runtime/orchestrator.go](runtime/orchestrator.go) |
| Orchestrator with session history passed to every agent, sequential and parallel sub-agent stages, fan-in policies (`fail_fast`, `wait_all`, `best_effort`), parallel tool calls for read-safe tools, resuming interrupted runs (also on startup), run queueing with backpressure, concurrency limits per tenant/workflow/agent/tool, idempotent run start, cancellation | [runtime/orchestrator.go](runtime/orchestrator.go) |
| Strict replay (rebuild terminal state from events) | [runtime/replay.go](runtime/replay.go) |
| REST + SSE API with `Last-Event-ID` resume and heartbeats; `/healthz` liveness and `/readyz` readiness (pings the store); read/idle timeouts and a 1 MiB request body limit (413) | [api/server.go](api/server.go) |
| Embedded responsive debug console (served with `-debug`) for session resume, chat-style run testing, setup inspection, REST checkpoints, request logs, and HTTP SSE | [api/web/index.html](api/web/index.html) |
| HTTP server example, logging JSON (`log/slog`) to stdout | [cmd/agent-loop/main.go](cmd/agent-loop/main.go) |
| Programmatic example: planner -> parallel researchers -> synthesizer + replay | [cmd/example-workflow/main.go](cmd/example-workflow/main.go) |

The MCP client, snapshot/compaction, and pgvector memory are still
doc-only. Both the in-memory store and the PostgreSQL driver implement
the aggregate `store.Store` interface, so `runtime` and `api` are
driver-agnostic.

## Run it

Default test suite: unit tests, fuzz seeds and end-to-end scenarios
replayed from recorded `gemma4:e2b-it-qat` output. It needs no external
services and runs in CI:

```sh
make test   # go test -race -count=1 ./...
```

[.claude/skills/testing/SKILL.md](.claude/skills/testing/SKILL.md) explains
which tier a test belongs in, and how to fuzz (`make fuzz`) and re-record the
scenario fixtures from a live model (`make record-docker-ollama` or
`make record-host-ollama`).

Programmatic workflow demo:

```sh
go run ./cmd/example-workflow
```

HTTP server with mock provider:

```sh
go run ./cmd/agent-loop -debug
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

The console and the debug-only setup endpoints below are served only with
`-debug`, because they expose agent instructions:

```sh
curl http://localhost:8080/v1/debug/agents
curl http://localhost:8080/v1/debug/tools
```

PostgreSQL integration tests (behind the `pg` build tag):

```sh
docker run -d --rm --name agentloop-pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=agentloop \
  -p 5432:5432 postgres:18-alpine

AGENT_LOOP_PG_DSN='postgres://postgres:postgres@localhost:5432/agentloop?sslmode=disable' \
  go test -tags=pg -race -count=1 ./store/pg/...
```

The driver embeds the numbered migrations in
[store/pg/migrations/](store/pg/migrations/). `Migrate(ctx)`, which
`cmd/agent-loop` runs at startup, applies the ones newer than the latest
version in `schema_migrations`. A schema change is a new numbered file;
applied files are never edited. Without `AGENT_LOOP_PG_DSN` set the tests
skip cleanly.

Docker Compose REST integration stack:

Quick shortcuts are available in [Makefile](Makefile): `make api-local-docker-ollama`,
`make api-local-host-ollama`, `make test-local`,
`make test-stack-docker-ollama`, and `make test-stack-host-ollama`.

```sh
# Host ports are prefixed with 4 to avoid common local conflicts:
# API http://localhost:48080, Postgres localhost:45432, Ollama localhost:41434
OLLAMA_MODEL=gemma4:e2b-it-qat \
   docker compose -f docker-compose.test.yml up --build \
   --abort-on-container-exit --exit-code-from rest-test rest-test

docker compose -f docker-compose.test.yml down
```

If `docker compose ps` does not show `api` immediately, check
`docker compose -f docker-compose.test.yml ps -a`: the API intentionally
stays in `Created` until `ollama-pull` exits successfully. Pulling
`gemma4:e2b-it-qat` can download a multi-GB model layer on first run.

The compose stack starts PostgreSQL 18, can talk to either the bundled
`ollama/ollama:latest` service or an external Ollama endpoint, pulls the
configured model, runs the API against Postgres and Ollama, and then
executes the REST integration test with the `rest` build tag.

Default bundled Ollama flow:

```sh
OLLAMA_MODEL=gemma4:e2b-it-qat \
   docker compose -f docker-compose.test.yml up --build \
   --abort-on-container-exit --exit-code-from rest-test ollama rest-test
```

On macOS, the bundled Dockerized Ollama instance runs inside Docker's Linux
VM and does not use Apple GPU acceleration. For faster local testing, run
Ollama natively on the host so it can use Metal, then point the test stack
at `host.docker.internal`:

```sh
ollama serve
ollama pull gemma4:e2b-it-qat

OLLAMA_MODEL=gemma4:e2b-it-qat \
OLLAMA_HOST=http://host.docker.internal:11434 \
OLLAMA_BASE_URL=http://host.docker.internal:11434 \
   docker compose -f docker-compose.test.yml up --build \
   --abort-on-container-exit --exit-code-from rest-test postgres ollama-pull rest-test
```

The API server also understands these local testing environment variables
outside Compose:

```sh
AGENT_LOOP_PG_DSN='postgres://postgres:postgres@localhost:45432/agentloop?sslmode=disable' \
OLLAMA_BASE_URL='http://localhost:41434' \
OLLAMA_MODEL='gemma4:e2b-it-qat' \
OLLAMA_TIMEOUT_SECONDS=900 \
go run ./cmd/agent-loop -addr :8080 -debug

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
