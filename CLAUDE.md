## Go
- Before every commit run what CI runs: `gofmt -l .` (must print nothing), `go vet ./...`,
  `go vet -tags=pg ./...`, `go build ./...`, `go test -race -count=1 ./...` and
  `go run ./cmd/example-workflow`. Fix the code rather than suppressing a finding.
- Tests behind build tags need services: `-tags=pg` needs `AGENT_LOOP_PG_DSN` (`make pg-up` gives
  one on `localhost:45432`); `-tags=rest` runs against a live API (`make test-local`,
  `make test-stack-docker-ollama`). Run the ones covering the packages you changed.
- Layout: binaries in `cmd/`; shared domain types in `core`; orchestration, scheduling and replay
  in `runtime`; persistence ports in `store` with Postgres in `store/pg`; model providers in
  `model`; tools in `tool`; authorization, budgets and approvals in `policy`; REST/SSE and the web
  UI in `api`. Dependencies point inward to `core` and the ports; `runtime` never imports
  `store/pg` or `api`, and only `cmd/` and `scenario/` import `model/ollama`. Tool metadata
  (`core.ToolDefinition`) lives in `core`; `tool` holds the registry and executors.
- The runtime is concurrency-safe and cancellation-aware: every blocking call takes a
  `context.Context` and returns when it is done. Protocol and state-machine logic goes in
  `runtime`/`policy`, never in `api`.
- Test tiers, fixtures and the test model are described in `.claude/skills/testing/SKILL.md`.
- Tests drive the real code through its ports and assert on stored events and results. Use the
  scripted `model.MockProvider` and `store.MemStore` rather than ad-hoc mocks. Fuzz every decoder of
  external input (model output, tool arguments, REST bodies). Each acceptance scenario in
  `docs/04-contracts-and-tests.md` that a change implements gets a test in the same change, and a
  bug gets a failing regression test before the fix.

## Comments
- Default to no comments. Write code whose names and structure make intent clear.
- Only comment a constraint the code cannot express: a workaround for an external bug,
  a non-obvious invariant, a reason something that looks wrong is intentional.
- Never comment what the next line does, never leave "changed X to Y" or "added for task Z"
  notes. That history goes in the commit message.
- Exported identifiers get a one-line doc comment that starts with their name; nothing more.
- When editing existing code, don't add comments to lines you didn't need to change.

## Documents
- `requirement.md` is what the framework must do. Under `docs/`: `01-domain-model.md` the
  entities and state machines, `02-postgres-schema.sql` the schema (kept in step with
  `store/pg/schema.sql`), `03-go-interfaces.md` the package contracts, `04-contracts-and-tests.md`
  the external contracts and acceptance scenarios. `README.md` says what is implemented and how to
  run it.
- Docs and code state current facts, arranged so they are easiest to read and understand.
  Git history keeps the past; don't preserve it in the text. A reversed decision is rewritten
  as the new decision.
- When a doc needs updating, consider rewriting the whole doc or section instead of patching
  it or appending a block. Appended blocks drift into a changelog.
- Before adding content, check for duplication elsewhere in the doc (and related docs).
  Merge or remove duplicates, and rewrite when that gives a clearer result.

## Tech debt
- `docs/tech-debts.md` lists every workaround, version pin and pre-release dependency,
  each with why it exists and what must be true to remove it. Create it with the first entry.
- When you add one, add its entry in the same change. When you touch code or a dependency
  an entry covers, check whether its exit condition is met.
- When an entry is resolved, remove the workaround and delete the entry. Don't mark it
  done or move it to a "resolved" list; git history keeps that.
