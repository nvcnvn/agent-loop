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

## The test model

`gemma4:e2b-it-qat` (about 4.3 GB, 128K context, tool calling and thinking) is
the smallest Gemma 4 that still calls tools reliably enough for the scenarios.
The exact quant is pinned so fixtures are reproducible. Override with
`OLLAMA_MODEL=...` to try another model; scenario names and assertions don't
depend on it.

## Scenarios and fixtures

`scenario/scenario_test.go` runs real workflows through `runtime.Orchestrator`.
`provider(t)` returns:
- by default, a `model.Replayer` loaded from `scenario/testdata/<TestName>.json`;
- with `-record`, a `model.Recorder` around the live Ollama provider. It saves
  the fixture only if the scenario passed.

The Replayer matches each request on its messages, tools, schema and
parameters, ignoring IDs and the model name. Identical requests are answered
in recorded order, so parallel stages replay safely. A scenario fails with
`model.ErrNoFixture` when:
- the runtime sends a request that was never recorded (prompt or tool-spec
  drift), or
- the run ends with recorded responses unused (fewer model calls than before).

If the drift is intended, re-record and review the fixture diff.

Adding a scenario:
1. Write `TestXxx` calling `provider(t)` and `run(t, agents, stages...)`.
   Assert on facts a correct agent must produce (tool outputs from events,
   key tokens in the answer), never on exact model wording.
2. `make record-host-ollama` (or `-docker-`) writes `scenario/testdata/TestXxx.json`.
3. Run `make test` and commit the fixture with the code.

A small model can fail a live run that is otherwise correct. Re-run once; if it
keeps failing, tighten the agent instructions rather than loosening the
assertions.
