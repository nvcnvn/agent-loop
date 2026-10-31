---
name: testing
description: How to test agent-loop. Covers which tier a test belongs in (unit, fuzz, fixture-replayed scenario, live Docker integration), how to record model fixtures with a small local model, and the commands for each tier. Use when writing or changing tests, adding a scenario, re-recording fixtures, fixing a bug (regression test first), or when asked how to run the slow integration tests.
---

# Testing agent-loop

Default `go test ./...` must never touch the network. Real model behaviour
reaches it as recorded fixtures; live services are opt-in through build tags
or the `-record` flag.

## Pick the tier

| What changed | Tier | Where | Driven by |
|---|---|---|---|
| Logic-dense code: state machines, budgets, policy, loop detection, scheduling, replay, registries | Unit | `<pkg>/*_test.go` | `model.MockProvider`, `store.MemStore`, `httptest` |
| A decoder of external input: model output, tool arguments, REST bodies | Fuzz | `Fuzz*` next to the unit tests | seed corpus with real and hostile samples |
| How agents behave on real model output: prompts, tool-call loop, multi-stage workflows | Scenario | `scenario/` | `model.Replayer` over `scenario/testdata/<Test>.json` |
| Adapter wire format or a model upgrade | Record | `make record-docker-ollama` | live Ollama, rewrites the fixtures |
| Postgres store | Integration | `store/pg`, `-tags=pg` | `make pg-up`, `AGENT_LOOP_PG_DSN` |
| Full REST/SSE stack | Integration | `api`, `-tags=rest` | `make test-stack-docker-ollama` |

Rules:
- Assert on stored events and results through the ports, not on internals.
  Use the scripted `model.MockProvider` and `store.MemStore`, never ad-hoc mocks.
- A bug gets a failing test before the fix. For decoders, add the crashing
  input as an `f.Add` seed so it runs on every `go test`.
- Each acceptance scenario in `docs/04-contracts-and-tests.md` that a change
  implements gets a test in the same change.
- Only `cmd/` and `scenario/` may import `model/ollama`. Everything else uses
  `model.Provider`.

## Commands

```sh
make test                  # unit + fuzz seeds + replayed scenarios, offline, -race
make fuzz FUZZTIME=1m      # fuzz every Fuzz* target in turn
make record-docker-ollama  # start Dockerized Ollama, pull the model, record scenarios
make record-host-ollama    # same against host Ollama (Metal on macOS, much faster)
make pg-up && AGENT_LOOP_PG_DSN=postgres://postgres:postgres@localhost:45432/agentloop?sslmode=disable go test -tags=pg ./store/pg
make test-stack-docker-ollama   # REST suite against API + Postgres + Ollama in Compose
```

## The test models

Every scenario runs once per model in `models` in `scenario/scenario_test.go`,
each with its own fixtures in `scenario/testdata/<model>/`. Small models from
different families catch prompts and runtime behaviour that only work with one
of them:

| Model | Size | Capabilities |
|---|---|---|
| `gemma4:e2b-it-qat` | ~4.3 GB | vision, audio, tools, thinking; miscounts shapes in most tries, so re-run the image scenarios when recording it |
| `qwen3-vl:2b` | ~1.9 GB | vision, tools, thinking |
| `ministral-3:3b` | ~3 GB | vision, tools |

Exact tags are pinned so fixtures are reproducible. Record one model at a
time and start host Ollama with a small context and a single loaded model:
`OLLAMA_CONTEXT_LENGTH=8192 OLLAMA_MAX_LOADED_MODELS=1 ollama serve`. Ollama
reserves memory for the model's whole context, so at its default 256K context
`qwen3-vl:2b` takes about 31 GB, and two loaded models can exhaust a laptop. Vision scenarios
(`forEachModel(t, true, ...)`) run only for models marked `vision`. Check what
a model supports with `curl localhost:11434/api/show -d '{"model":"<tag>"}'`.

## What the scenarios cover

The scenarios play a small support desk: deterministic native tools for a CRM,
orders (refunds need approval), inventory, prices, a knowledge base and a style
checker (`scenario/world_test.go`), and a notes MCP server over streamable
HTTP. Each guards features a real model must drive:

| Scenario | Features |
|---|---|
| `TestPlainAnswer` | one model call, instructions |
| `TestToolCallRoundTrip` | one tool call and its result |
| `TestParallelAgentsThenSynthesis` | parallel stage, stage outputs in a later prompt |
| `TestSupportRefund` | tool chain with IDs flowing between calls, approval-gated write tool (approved) |
| `TestRefundRejected` | rejected approval returned to the agent |
| `TestToolErrorRecovery` | tool error returned to the model, which retries with fixed input |
| `TestDelegatedResearch` | `delegate` tool, child task using its own tool |
| `TestDraftReviewHandBack` | `hand_back` with `{{feedback}}`; the first draft always goes back |
| `TestOrderExtractionThenQuote` | `output_schema`, parallel tool calls in one response |
| `TestTriageGraph` | enum output, `stop_when`, `depends_on` branches and join |
| `TestSessionMemory` | session history across runs |
| `TestMCPNotes` | MCP tool over streamable HTTP |
| `TestImageQuestion`, `TestImageCountThenTool`, `TestImageToJSONThenTool` | image input, alone and with tools and `output_schema` (vision models) |

`world.decision` sets how a scenario's approval requests are resolved; an
unexpected approval fails the scenario.

## Scenarios and fixtures

`scenario/scenario_test.go` runs real workflows through `runtime.Orchestrator`.
`forEachModel` gives each model's subtest a provider:
- by default, a `model.Replayer` loaded from
  `scenario/testdata/<model>/<TestName>.json`;
- with `-record`, the subtests of `OLLAMA_MODEL` get a `model.Recorder` around
  the live Ollama provider, which saves the fixture only if the scenario
  passed; the other models still replay.

The Replayer matches each request on its messages (including images and
thinking), tools, schema and parameters, ignoring IDs and the model name.
Identical requests are answered in recorded order, so parallel stages replay
safely. A scenario fails with `model.ErrNoFixture` when:
- the runtime sends a request that was never recorded (prompt or tool-spec
  drift), or
- the run ends with recorded responses unused (fewer model calls than before).

If the drift is intended, re-record every model whose fixtures it touches
(`make record-host-ollama OLLAMA_MODEL=<tag>`, or narrow it with
`OLLAMA_MODEL=<tag> go test -run 'TestXxx/<model>' ./scenario -args -record`)
and review the fixture diffs.

Adding a scenario:
1. Write `TestXxx` with `forEachModel`, calling `run` (or `runWithImages`).
   Assert on facts a correct agent must produce (tool outputs from events,
   key tokens in the answer), never on exact model wording.
2. Record it for every model: `make record-host-ollama OLLAMA_MODEL=<tag>`.
3. Run `make test` and commit the fixtures with the code.

Adding a model: add it to `models`, then record all scenarios for it.

A small model can fail a live run that is otherwise correct. Re-run once; if it
keeps failing, tighten the agent instructions rather than loosening the
assertions, and re-record the other models, whose requests then change too.
