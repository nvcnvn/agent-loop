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
| PostgreSQL driver (pgx v5, pgxpool, versioned embedded migrations, polling Subscribe); needs the pgvector extension (`pgvector/pgvector:pg18`) | [store/pg/pg.go](store/pg/pg.go) |
| Provider-agnostic model abstraction (`Provider` interface) | [model/model.go](model/model.go) |
| Mock provider for deterministic tests | [model/mock.go](model/mock.go) |
| Fixture recorder and request-matching replayer for real-model regression tests | [model/fixture.go](model/fixture.go) |
| Ollama provider (`/api/chat`) | [model/ollama/ollama.go](model/ollama/ollama.go) |
| OpenAI-compatible provider (`/chat/completions`; GLM, OpenAI, vLLM): tool-name mapping, reasoning, cached tokens, cost from configured prices | [model/openai/openai.go](model/openai/openai.go) |
| Retry with jittered backoff and `Retry-After` for transient provider errors | [model/retry.go](model/retry.go) |
| Streaming replies from both adapters; SSE `agent_message_delta` events with `?deltas=1` | [runtime/deltas.go](runtime/deltas.go), [api/server.go](api/server.go) |
| Native Go tool registry + builtin demo tools (`echo`, `uppercase`, `math.add`) | [tool/tool.go](tool/tool.go) |
| Tool policy (allow / deny / approval, by tool name or side-effect level; `tool_policy` in the config file) | [policy/policy.go](policy/policy.go) |
| Budget manager (model calls, tool calls, sub-agents, tokens, cost) | [policy/policy.go](policy/policy.go) |
| Prometheus metrics at `/metrics` (runs, model calls, tokens, cost, tools, queue depth, HTTP latency) | [observe/metrics.go](observe/metrics.go) |
| Loop / stall detector; idle-wait diagnostics (`idle_wait` events) | [policy/loop.go](policy/loop.go), [runtime/orchestrator.go](runtime/orchestrator.go) |
| Workflows as stage graphs (`depends_on`) with hand-backs to earlier stages, stop conditions, retries, stage/agent/tool timeouts, stage approval gates, and JSON Schema for input and stage outputs; agents delegating to child agents at runtime (`delegate` tool, depth-limited); agents and named workflow templates loaded from a JSON config | [runtime/workflow.go](runtime/workflow.go), [runtime/config.go](runtime/config.go) |
| Orchestrator with session history passed to every agent, sequential and parallel sub-agent stages, fan-in policies (`fail_fast`, `wait_all`, `best_effort`, `quorum`), parallel tool calls for read-safe tools, resuming interrupted runs (also on startup), run queueing with backpressure, concurrency limits per tenant/workflow/agent/tool, idempotent run start, cancellation | [runtime/orchestrator.go](runtime/orchestrator.go) |
| Long-term memory: `memory.save` / `memory.search` tools over embeddings (Ollama or OpenAI-compatible), scoped by session, workflow or tenant, with provenance; pgvector in Postgres | [tool/memory/memory.go](tool/memory/memory.go), [store/memory.go](store/memory.go), [store/pg/memory.go](store/pg/memory.go) |
| Session history compaction: summaries of older messages by a configured agent, at the start of a run | [runtime/compaction.go](runtime/compaction.go) |
| Strict replay (rebuild terminal state from events) | [runtime/replay.go](runtime/replay.go) |
| Re-execution of a finished run from its recorded model requests/responses and tool results (`POST /v1/runs/{rid}/reexecute`) | [runtime/reexecute.go](runtime/reexecute.go) |
| REST + SSE API with `Last-Event-ID` resume and heartbeats; `/healthz` liveness and `/readyz` readiness (pings the store); read/idle timeouts and a 1 MiB request body limit (413) | [api/server.go](api/server.go) |
| Embedded responsive debug console (served with `-debug`) for session resume, chat-style run testing, setup inspection, REST checkpoints, request logs, and HTTP SSE | [api/web/index.html](api/web/index.html) |
| MCP tools: servers over stdio or streamable HTTP, tools discovered and translated at startup, auth headers from the environment, reconnects, retries of idempotent calls, a circuit breaker | [tool/mcp/mcp.go](tool/mcp/mcp.go) |
| OpenTelemetry traces of runs, stages, tasks (including delegated ones), model calls, tool calls and approval waits | [runtime/orchestrator.go](runtime/orchestrator.go) |
| HTTP server example: JSON logs (`log/slog`) to stdout, resumes unfinished runs at startup, graceful shutdown on SIGINT/SIGTERM (`-shutdown-timeout`) | [cmd/agent-loop/main.go](cmd/agent-loop/main.go) |
| Programmatic example: planner -> parallel researchers -> synthesizer + replay | [cmd/example-workflow/main.go](cmd/example-workflow/main.go) |

Both the in-memory store and the PostgreSQL driver implement
the aggregate `store.Store` interface, so `runtime` and `api` are
driver-agnostic.

## Run it

Default test suite: unit tests, fuzz seeds and end-to-end scenarios
replayed from output recorded on `gemma4:e2b-it-qat`, `qwen3-vl:2b`,
`ministral-3:3b` and `z-ai/glm-5.3`, including image input. It needs no external
services and runs in CI:

```sh
make test   # go test -race -count=1 ./...
```

[.claude/skills/testing/SKILL.md](.claude/skills/testing/SKILL.md) explains
which tier a test belongs in, and how to fuzz (`make fuzz`) and re-record the
scenario fixtures from a live model (`make record-docker-ollama`,
`make record-host-ollama`, or `make record-openai` for a hosted API).

Programmatic workflow demo:

```sh
go run ./cmd/example-workflow
```

HTTP server with mock provider:

```sh
go run ./cmd/agent-loop -debug -trust-identity-headers
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

Without `-config` the server has one `echo` agent. `-config agents.json` replaces it with the agents,
named workflow templates and MCP servers in that file, and a run can then start a template by name:

```sh
curl -X POST http://localhost:8080/v1/runs \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"<id>","template":"research","input":{"question":"Why is the sky blue?"}}'
```

The workflow and config formats are in
[docs/04-contracts-and-tests.md](docs/04-contracts-and-tests.md) §3 to §5. A failed or timed-out run
can be resumed with `POST /v1/runs/<run_id>/resume`.

To export traces, point the server at an OTLP/HTTP collector, for example a local Jaeger:

```sh
docker run -d --rm -p 16686:16686 -p 4318:4318 jaegertracing/jaeger:2.10.0
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 OTEL_SERVICE_NAME=agent-loop \
  go run ./cmd/agent-loop -trust-identity-headers
# traces at http://localhost:16686
```

The server refuses to start until you choose how `/v1` requests are
authenticated. In production set `AGENT_LOOP_API_KEYS` to comma-separated
`<tenant>:<principal>:<key>` entries, where tenant and principal are UUIDs; clients send `Authorization: Bearer
<key>` (SSE clients may pass `?access_token=<key>` instead), and the key alone
decides the tenant and principal. `-trust-identity-headers` takes them from
`X-Tenant-ID` and `X-Principal-ID` (UUIDs) unchecked, for local development only.
`/healthz`, `/readyz` and `/metrics` need no key. Any key of a tenant may act
on everything in that tenant.

The debug console is served by the same Go binary and has no separate
frontend build step. It can create or resume sessions, send human
messages, start agent runs, inspect current agent and tool setup, poll
run state, list run events, cancel runs, resolve approval IDs, call
`/healthz`, and subscribe to run events through HTTP SSE with
`Last-Event-ID` resume. When the server checks API keys, enter one in the
console's key field; it is kept in the browser's local storage. The console also includes REST/SSE checkpoint
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
  -p 5432:5432 pgvector/pgvector:pg18

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
go run ./cmd/agent-loop -addr :8080 -debug -trust-identity-headers

# or a hosted OpenAI-compatible API instead of Ollama (cp .env.example .env and fill it in;
# OLLAMA_EMBED_MODEL or LLM_EMBED_MODEL there also enables the memory.save/memory.search tools):
set -a && . ./.env && set +a && go run ./cmd/agent-loop -debug -trust-identity-headers

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
5. **Replay scope** — strict replay rebuilds the terminal state from
   events; re-execution runs the run again on its recorded model and
   tool outputs and fails on the first request that was never recorded.
6. **Concurrent tool group ordering** — parallel-safe tool calls are
   dispatched concurrently but ordered by ordinal before reply messages
   are appended, so persisted history is deterministic.
7. **Single-tenant ergonomics** — `core.DefaultTenant` and
   `core.DefaultPrincipal` let single-tenant deployments ignore tenant
   headers; the data model still carries tenant IDs everywhere.
