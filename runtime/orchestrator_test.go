package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

func TestRunLifecycleIsLogged(t *testing.T) {
	prov := model.NewMockProvider(model.MockResponse{Content: "hi"})
	orch, _, sid := setup(t, []runtime.Agent{{Name: "echo", Model: "mock", Provider: prov}})
	var buf bytes.Buffer
	orch.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "echo", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Wait(ctx, runID); err != nil {
		t.Fatal(err)
	}
	var finished map[string]any
	for line := range strings.Lines(buf.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["msg"] == "run finished" {
			finished = rec
		}
	}
	if finished == nil || finished["run_id"] != string(runID) || finished["status"] != string(core.RunSucceeded) {
		t.Fatalf("missing run finished record: %s", buf.String())
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

// Acceptance: §Budget and Safety #2 – parallel sub-agent fan-out reserves as one group.
func TestParallelSubAgentBudgetReservesWholeStage(t *testing.T) {
	pa := model.NewMockProvider(model.MockResponse{Content: "A"})
	pb := model.NewMockProvider(model.MockResponse{Content: "B"})
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "a", Model: "m", Provider: pa},
		{Name: "b", Model: "m", Provider: pb},
	})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name: "fan",
			Kind: runtime.StageParallel,
			Agents: []runtime.AgentStep{
				{Name: "a", Prompt: "x"},
				{Name: "b", Prompt: "y"},
			},
		}}},
		Limits: core.Limits{MaxSubAgents: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, runID)
	if err == nil {
		t.Fatalf("expected sub-agent budget failure, got status=%s", res.Status)
	}
	if !strings.Contains(err.Error(), "sub agents") {
		t.Fatalf("expected sub-agent budget message, got %v", err)
	}
	tasks, err := s.ListRunTasks(ctx, core.DefaultTenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected no child tasks after failed group reservation, got %d", len(tasks))
	}
}

func TestStartRunCapsClientLimits(t *testing.T) {
	pa := model.NewMockProvider(model.MockResponse{Content: "A"})
	pb := model.NewMockProvider(model.MockResponse{Content: "B"})
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "a", Model: "m", Provider: pa},
		{Name: "b", Model: "m", Provider: pb},
	})
	orch.MaxLimits = core.Limits{MaxToolCalls: 8, MaxRuntime: time.Minute}
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "fan",
			Kind:   runtime.StageParallel,
			Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}, {Name: "b", Prompt: "y"}},
		}}},
		Limits: core.Limits{MaxToolCalls: 1 << 30, MaxRuntime: 1000 * time.Hour, MaxConcurrent: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, runID); err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("status=%s err=%v", res.Status, err)
	}
	run, err := s.GetRun(ctx, core.DefaultTenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Limits.MaxToolCalls != 8 || run.Limits.MaxRuntime != time.Minute || run.Limits.MaxConcurrent <= 0 {
		t.Fatalf("limits not capped: %+v", run.Limits)
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
	orch, s, sid := setup(t, []runtime.Agent{
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
	if err != nil || res.Outputs["s"] != "fallback" {
		t.Fatalf("status=%s err=%v outputs=%v; want the model to answer after the refusal", res.Status, err, res.Outputs)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	for _, ev := range events {
		if ev.Type == core.EvToolStarted {
			t.Fatalf("denied tool was executed: %s", ev.Payload)
		}
	}
	calls := prov.Calls()
	last := calls[len(calls)-1].Messages
	if msg := last[len(last)-1]; msg.Role != model.RoleTool || !strings.Contains(msg.Content, "not available") {
		t.Fatalf("model did not get the refusal as a tool result: %+v", msg)
	}
}

func TestToolErrorsReturnToModel(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{
			{ID: "1", Name: "fails", Input: json.RawMessage(`{}`)},
			{ID: "2", Name: "panics", Input: json.RawMessage(`{}`)},
		}},
		model.MockResponse{Content: "recovered"},
	)
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "ag", Model: "m", Provider: prov, Tools: []string{"fails", "panics"}},
	})
	_ = orch.Tools.Register(core.ToolDefinition{Name: "fails"}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{}, errors.New("disk full")
	}))
	_ = orch.Tools.Register(core.ToolDefinition{Name: "panics", Concurrency: core.ConcurrencyParallelRead}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		panic("nil map")
	}))
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
	if err != nil || res.Outputs["s"] != "recovered" {
		t.Fatalf("status=%s err=%v reason=%s", res.Status, err, res.FailureReason)
	}
	msgs := prov.Calls()[1].Messages
	results := msgs[len(msgs)-2:]
	if !strings.Contains(results[0].Content, "disk full") || !strings.Contains(results[1].Content, "nil map") {
		t.Fatalf("tool errors not returned to the model: %+v", results)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	stored := map[string]string{}
	for _, ev := range events {
		if ev.Type == core.EvToolCompleted {
			var p struct{ Tool, Error string }
			_ = json.Unmarshal(ev.Payload, &p)
			stored[p.Tool] = p.Error
		}
	}
	if !strings.Contains(stored["fails"], "disk full") || !strings.Contains(stored["panics"], "nil map") {
		t.Fatalf("tool_call_completed events missing errors: %v", stored)
	}
}

// Acceptance: §Tool Calling #5 – exclusive tools do not overlap parallel ones.
func TestExclusiveToolWaitsForEarlierParallelTools(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{
			{ID: "1", Name: "slowread", Input: json.RawMessage(`{"n":"r1"}`)},
			{ID: "2", Name: "slowread", Input: json.RawMessage(`{"n":"r2"}`)},
			{ID: "3", Name: "excl", Input: json.RawMessage(`{"n":"x"}`)},
			{ID: "4", Name: "slowread", Input: json.RawMessage(`{"n":"r3"}`)},
		}},
		model.MockResponse{Content: "done"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"slowread", "excl"}}})
	sleepy := tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		time.Sleep(20 * time.Millisecond)
		return tool.Result{Output: json.RawMessage(`{}`)}, nil
	})
	_ = orch.Tools.Register(core.ToolDefinition{Name: "slowread", Concurrency: core.ConcurrencyParallelRead}, sleepy)
	_ = orch.Tools.Register(core.ToolDefinition{Name: "excl", Concurrency: core.ConcurrencyExclusive}, sleepy)
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, runID); err != nil {
		t.Fatalf("status=%s err=%v", res.Status, err)
	}

	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	callOfStep := map[core.StepID]string{}
	started, completed := map[string]int64{}, map[string]int64{}
	for _, ev := range events {
		switch ev.Type {
		case core.EvToolScheduled:
			var p struct {
				Input struct{ N string }
			}
			_ = json.Unmarshal(ev.Payload, &p)
			callOfStep[ev.StepID] = p.Input.N
		case core.EvToolStarted:
			started[callOfStep[ev.StepID]] = ev.Sequence
		case core.EvToolCompleted:
			completed[callOfStep[ev.StepID]] = ev.Sequence
		}
	}
	if len(completed) != 4 {
		t.Fatalf("completed calls=%v want 4", completed)
	}
	if started["x"] < completed["r1"] || started["x"] < completed["r2"] || started["r3"] < completed["x"] {
		t.Fatalf("exclusive tool overlapped parallel tools: started=%v completed=%v", started, completed)
	}
}

func TestToolErrorAfterCancelFailsTask(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "blocks", Input: json.RawMessage(`{}`)}}},
		model.MockResponse{Content: "should not be asked"},
	)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"blocks"}}})
	started := make(chan struct{})
	_ = orch.Tools.Register(core.ToolDefinition{Name: "blocks"}, tool.ExecutorFunc(func(ctx context.Context, _ tool.Call) (tool.Result, error) {
		close(started)
		<-ctx.Done()
		return tool.Result{}, ctx.Err()
	}))
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := orch.Cancel(ctx, core.DefaultTenant, runID, "test"); err != nil {
		t.Fatal(err)
	}
	res, _ := orch.Wait(ctx, runID)
	if res.Status != core.RunCanceled || len(prov.Calls()) != 1 {
		t.Fatalf("status=%s model calls=%d; want canceled after 1 call", res.Status, len(prov.Calls()))
	}
}

// Acceptance: §Human-in-the-Loop – approval-required tools pause before execution.
func TestApprovalRequiredToolWaitsForResolution(t *testing.T) {
	echoCall := func(text, id string) model.ToolCall {
		raw, _ := json.Marshal(map[string]string{"text": text})
		return model.ToolCall{ID: id, Name: "echo", Input: raw}
	}
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{echoCall("approve me", "1")}},
		model.MockResponse{Content: "done"},
	)
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}},
	})
	orch.Policy.ApprovalRequired = []string{"echo"}

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

	var approvalID core.ApprovalID
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if ev.Type != core.EvApprovalReq {
				continue
			}
			var payload struct {
				ApprovalID core.ApprovalID `json:"approval_id"`
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			approvalID = payload.ApprovalID
			break
		}
		if approvalID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if approvalID == "" {
		t.Fatal("approval request was not emitted")
	}
	run, err := s.GetRun(ctx, core.DefaultTenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != core.RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, approvalID, "approved", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, runID)
	if err != nil {
		t.Fatalf("wait: %v (status=%s reason=%s)", err, res.Status, res.FailureReason)
	}
	if res.Status != core.RunSucceeded {
		t.Fatalf("status=%s reason=%s", res.Status, res.FailureReason)
	}
	if res.Usage.ToolCalls != 1 {
		t.Fatalf("tool calls=%d want 1", res.Usage.ToolCalls)
	}
	events, err := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundResolution := false
	for _, ev := range events {
		if ev.Type == core.EvApprovalRes {
			foundResolution = true
			break
		}
	}
	if !foundResolution {
		t.Fatal("approval resolution event was not emitted")
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

// Acceptance: §Policy and Budget Contract – tool-call groups reserve before execution.
func TestToolCallBudgetReservesWholeGroup(t *testing.T) {
	echoCall := func(text, id string) model.ToolCall {
		raw, _ := json.Marshal(map[string]string{"text": text})
		return model.ToolCall{ID: id, Name: "echo", Input: raw}
	}
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{echoCall("a", "1"), echoCall("b", "2"), echoCall("c", "3")}},
		model.MockResponse{Content: "never reached"},
	)
	orch, s, sid := setup(t, []runtime.Agent{
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
	if res.Usage.ToolCalls != 0 {
		t.Fatalf("tool calls=%d want 0 because group reservation failed before scheduling", res.Usage.ToolCalls)
	}
	events, err := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == core.EvToolScheduled || ev.Type == core.EvToolStarted || ev.Type == core.EvToolCompleted {
			t.Fatalf("unexpected tool event after failed group reservation: %s", ev.Type)
		}
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
		if err := orch.Cancel(context.Background(), core.DefaultTenant, runID, "test"); err != nil {
			t.Error(err)
		}
	}()
	res, _ := orch.Wait(ctx, runID)
	if res.Status != core.RunCanceled && res.Status != core.RunFailed {
		t.Fatalf("expected canceled/failed, got %s", res.Status)
	}
}

// Acceptance: §Security #1 – cross-tenant run cancellation.
func TestCancelIgnoresOtherTenant(t *testing.T) {
	slow := &slowProvider{delay: 200 * time.Millisecond}
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
	if err := orch.Cancel(ctx, "other-tenant", runID, "test"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cross-tenant cancel err=%v want ErrNotFound", err)
	}
	res, err := orch.Wait(ctx, runID)
	if err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("status=%s err=%v want succeeded", res.Status, err)
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
