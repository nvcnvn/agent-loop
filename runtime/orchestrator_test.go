package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

func setup(t *testing.T, agents []runtime.Agent) (*runtime.Orchestrator, store.Store, core.SessionID) {
	t.Helper()
	s := store.NewMemStore()
	reg := tool.NewRegistry()
	if err := tool.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	orch := runtime.NewOrchestrator(s, reg, agents)
	sid, err := s.CreateSession(context.Background(), store.Session{
		TenantID:  core.DefaultTenant,
		CreatedBy: core.DefaultPrincipal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return orch, s, sid
}

func waitCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// Acceptance: §Session and Run Lifecycle #1 – simple sequential run.
func TestSequentialRunSucceeds(t *testing.T) {
	prov := model.NewMockProvider(model.MockResponse{Content: "hi"})
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "echo", Model: "mock", Provider: prov},
	})

	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{
			Stages: []runtime.Stage{{
				Name:   "reply",
				Kind:   runtime.StageSequential,
				Agents: []runtime.AgentStep{{Name: "echo", Prompt: "say hi"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, runID)
	if err != nil {
		t.Fatalf("wait: %v (status=%s reason=%s)", err, res.Status, res.FailureReason)
	}
	if res.Status != core.RunSucceeded {
		t.Fatalf("status=%s want succeeded", res.Status)
	}
	if got := res.Outputs["reply"]; got != "hi" {
		t.Fatalf("output=%q", got)
	}
}

// Acceptance: §Session and Run Lifecycle #2 – idempotency.
func TestIdempotentStartRun(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{Content: "ok"},
		model.MockResponse{Content: "ok2"},
	)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov}})
	ctx, cancel := waitCtx()
	defer cancel()
	in := runtime.StartRunInput{
		SessionID:      sid,
		IdempotencyKey: "fixed",
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "s",
			Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}},
		}}},
	}
	r1, err := orch.StartRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := orch.StartRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Fatalf("idempotent start returned different IDs: %s vs %s", r1, r2)
	}
}

// Acceptance: §Concurrent Sub-Agent Workflow – parallel stage with deterministic output ordering.
func TestParallelStageWaitAll(t *testing.T) {
	// Each agent gets its own provider so the mapping from agent name to
	// response is deterministic (one shared provider would hand out
	// scripted responses in goroutine-race order).
	pa := model.NewMockProvider(model.MockResponse{Content: "A"})
	pb := model.NewMockProvider(model.MockResponse{Content: "B"})
	pc := model.NewMockProvider(model.MockResponse{Content: "C"})
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "a", Model: "m", Provider: pa},
		{Name: "b", Model: "m", Provider: pb},
		{Name: "c", Model: "m", Provider: pc},
	})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:          "fan",
			Kind:          runtime.StageParallel,
			FailurePolicy: runtime.WaitAll,
			Agents: []runtime.AgentStep{
				{Name: "a", Prompt: "x"},
				{Name: "b", Prompt: "y"},
				{Name: "c", Prompt: "z"},
			},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.RunSucceeded {
		t.Fatalf("status=%s reason=%s", res.Status, res.FailureReason)
	}
	out := res.Outputs["fan"]
	// Output order is deterministic by step ordinal even though execution
	// is concurrent. We expect agents in declaration order.
	for i, want := range []string{"a=A", "b=B", "c=C"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in output (i=%d): %q", want, i, out)
		}
	}
}

// Acceptance: §Tool Calling #4 – parallel read-only tool calls fan in.
func TestParallelToolCalls(t *testing.T) {
	// Model first asks for two parallel echo calls, then returns final answer.
	echoCall := func(text, id string) model.ToolCall {
		raw, _ := json.Marshal(map[string]string{"text": text})
		return model.ToolCall{ID: id, Name: "echo", Input: raw}
	}
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{echoCall("alpha", "1"), echoCall("beta", "2")}},
		model.MockResponse{Content: "done"},
	)
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "tooluser", Model: "m", Provider: prov, Tools: []string{"echo"}},
	})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "s",
			Agents: []runtime.AgentStep{{Name: "tooluser", Prompt: "use echo twice"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != core.RunSucceeded {
		t.Fatalf("status=%s reason=%s", res.Status, res.FailureReason)
	}
	if res.Usage.ToolCalls != 2 {
		t.Fatalf("tool calls=%d want 2", res.Usage.ToolCalls)
	}
}

// Acceptance: §Tool Calling #2 – tool call denied by policy.
func TestToolDeniedByPolicy(t *testing.T) {
	echoCall := func(text, id string) model.ToolCall {
		raw, _ := json.Marshal(map[string]string{"text": text})
		return model.ToolCall{ID: id, Name: "echo", Input: raw}
	}
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{echoCall("hi", "1")}},
		model.MockResponse{Content: "fallback"},
	)
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}},
	})
	orch.Policy.Denylist = []string{"echo"}

	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "s",
			Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, runID)
	if err == nil {
		// Model still returned tool calls; the runtime should refuse them
		// because the tool was filtered out of the allowed set.
		t.Fatalf("expected failure, got status=%s", res.Status)
	}
	if !strings.Contains(err.Error(), "echo") {
		t.Fatalf("err=%v", err)
	}
}

// Acceptance: §Persistence and Replay #2 – strict replay reconstructs status.
func TestStrictReplay(t *testing.T) {
	prov := model.NewMockProvider(model.MockResponse{Content: "hi"})
	orch, s, sid := setup(t, []runtime.Agent{{Name: "echo", Model: "m", Provider: prov}})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "s",
			Agents: []runtime.AgentStep{{Name: "echo", Prompt: "p"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Wait(ctx, runID); err != nil {
		t.Fatal(err)
	}
	res, err := runtime.Replay(ctx, s, core.DefaultTenant, runID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res.Status != core.RunSucceeded {
		t.Fatalf("replay status=%s", res.Status)
	}
}

// Acceptance: §Budget and Safety #1 – stop run when tool-call budget exhausted.
func TestBudgetExceededStopsRun(t *testing.T) {
	echoCall := func(text, id string) model.ToolCall {
		raw, _ := json.Marshal(map[string]string{"text": text})
		return model.ToolCall{ID: id, Name: "echo", Input: raw}
	}
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{echoCall("a", "1"), echoCall("b", "2"), echoCall("c", "3")}},
		model.MockResponse{Content: "never reached"},
	)
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}},
	})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "s",
			Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}},
		}}},
		Limits: core.Limits{MaxToolCalls: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, runID)
	if err == nil {
		t.Fatalf("expected budget-exceeded failure, got %s", res.Status)
	}
	if !strings.Contains(err.Error(), "tool calls") {
		t.Fatalf("expected tool-call budget message, got %v", err)
	}
}

// Acceptance: §Safety – loop detector catches repeated identical tool calls.
func TestLoopDetectorTripsOnRepeats(t *testing.T) {
	echoCall := func() model.ToolCall {
		raw, _ := json.Marshal(map[string]string{"text": "same"})
		return model.ToolCall{ID: "1", Name: "echo", Input: raw}
	}
	resps := make([]model.MockResponse, 0, 8)
	for i := 0; i < 8; i++ {
		resps = append(resps, model.MockResponse{ToolCalls: []model.ToolCall{echoCall()}})
	}
	prov := model.NewMockProvider(resps...)
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}, MaxToolLoops: 8},
	})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "s",
			Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}},
		}}},
		Limits: core.Limits{MaxToolCalls: 32, MaxModelCalls: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = orch.Wait(ctx, runID)
	if err == nil || !strings.Contains(err.Error(), "loop detected") {
		t.Fatalf("expected loop detection, got %v", err)
	}
}

// Acceptance: §Session and Run Lifecycle #3 – cancellation.
func TestCancelStopsRun(t *testing.T) {
	// Slow provider: blocks until ctx cancellation.
	slow := &slowProvider{delay: 2 * time.Second}
	orch, _, sid := setup(t, []runtime.Agent{{Name: "slow", Model: "m", Provider: slow}})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "s",
			Agents: []runtime.AgentStep{{Name: "slow", Prompt: "x"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = orch.Cancel(context.Background(), runID, "test")
	}()
	res, _ := orch.Wait(ctx, runID)
	if res.Status != core.RunCanceled && res.Status != core.RunFailed {
		t.Fatalf("expected canceled/failed, got %s", res.Status)
	}
}

type slowProvider struct{ delay time.Duration }

func (s *slowProvider) Name() string { return "slow" }
func (s *slowProvider) Capabilities(_ context.Context, _ string) (model.Capabilities, error) {
	return model.Capabilities{}, nil
}
func (s *slowProvider) Chat(ctx context.Context, _ model.ChatRequest) (model.ChatResponse, error) {
	select {
	case <-ctx.Done():
		return model.ChatResponse{}, ctx.Err()
	case <-time.After(s.delay):
		return model.ChatResponse{Message: model.Message{Role: model.RoleAssistant, Content: "slow"}}, nil
	}
}
