package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/policy"
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
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
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

// Acceptance: §Session and Run Lifecycle #5 – agents see earlier session messages.
func TestRunSeesEarlierSessionMessages(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{Content: "first answer"},
		model.MockResponse{Content: "second answer"},
		model.MockResponse{Content: "reviewed"},
		model.MockResponse{Content: "capped"},
	)
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "a", Model: "m", Provider: prov, Instructions: "be brief"},
		{Name: "b", Model: "m", Provider: prov},
	})
	ctx, cancel := waitCtx()
	defer cancel()
	for _, m := range []store.MessageProjection{
		{SessionID: sid, Role: "user", Visibility: core.VisibilityParticipant, Content: "my name is Ada"},
		{SessionID: sid, Role: "assistant", Visibility: core.VisibilityParticipant, Content: "posted as assistant"},
	} {
		if err := s.AddMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	start := func(stage runtime.Stage) {
		t.Helper()
		runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{stage}}})
		if err != nil {
			t.Fatal(err)
		}
		if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
			t.Fatalf("status=%s err=%v", res.Status, err)
		}
	}
	start(runtime.Stage{Name: "s", Agents: []runtime.AgentStep{{Name: "a", Prompt: "hello"}}})
	start(runtime.Stage{Name: "s", Agents: []runtime.AgentStep{{Name: "a", Prompt: "what is my name?"}, {Name: "b", Prompt: "review"}}})

	render := func(msgs []model.Message) string {
		var parts []string
		for _, m := range msgs {
			parts = append(parts, m.Role+":"+m.Content)
		}
		return strings.Join(parts, " | ")
	}
	orch.MaxHistoryMessages = 1
	start(runtime.Stage{Name: "s", Agents: []runtime.AgentStep{{Name: "b", Prompt: "last"}}})

	calls := prov.Calls()
	for i, want := range []string{
		"system:be brief | user:my name is Ada | user:posted as assistant | user:hello",
		"system:be brief | user:my name is Ada | user:posted as assistant | assistant:first answer | user:what is my name?",
		"user:my name is Ada | user:posted as assistant | assistant:first answer | user:review",
		"assistant:reviewed | user:last",
	} {
		if got := render(calls[i].Messages); got != want {
			t.Fatalf("call %d messages:\n got %s\nwant %s", i, got, want)
		}
	}
}

func TestUsagePersistedDuringRunAndStateReleased(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "probe", Input: json.RawMessage(`{}`)}}},
		model.MockResponse{Content: "done"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"probe"}}})
	var midRun core.Usage
	_ = orch.Tools.Register(core.ToolDefinition{Name: "probe"}, tool.ExecutorFunc(func(ctx context.Context, c tool.Call) (tool.Result, error) {
		run, err := s.GetRun(ctx, c.TenantID, c.RunID)
		midRun = run.Usage
		return tool.Result{Output: json.RawMessage(`{}`)}, err
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
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}
	if midRun.ModelCalls != 1 || midRun.ToolCalls != 1 || midRun.SubAgents != 1 {
		t.Fatalf("usage stored mid-run=%+v", midRun)
	}
	if orch.ActiveRuns() != 0 {
		t.Fatalf("active runs=%d after the run ended", orch.ActiveRuns())
	}
	if u := orch.Budget.Snapshot(runID); u != (core.Usage{}) {
		t.Fatalf("budget still tracks the finished run: %+v", u)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil || res.Status != core.RunSucceeded || res.Outputs["s"] != "done" || res.Usage.ModelCalls != 2 {
		t.Fatalf("Wait on finished run: %+v err=%v", res, err)
	}
	if _, err := orch.Wait(ctx, "other-tenant", runID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cross-tenant Wait err=%v", err)
	}
}

// Acceptance: §Observability and Operations #4 – backpressure.
func TestRunsQueueThenOverload(t *testing.T) {
	release := make(chan struct{})
	orch, s, sid := setup(t, []runtime.Agent{{Name: "gated", Model: "m", Provider: gateProvider(release)}})
	orch.MaxActiveRuns, orch.MaxQueuedRuns = 1, 1
	ctx, cancel := waitCtx()
	defer cancel()
	start := func(maxRuntime time.Duration) (core.RunID, error) {
		return orch.StartRun(ctx, runtime.StartRunInput{
			SessionID: sid,
			Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}}},
			Limits:    core.Limits{MaxRuntime: maxRuntime},
		})
	}
	first, err := start(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for {
		run, err := s.GetRun(ctx, core.DefaultTenant, first)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == core.RunRunning {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("first run never started")
		}
		time.Sleep(time.Millisecond)
	}
	second, err := start(50 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := start(time.Minute); !errors.Is(err, core.ErrOverloaded) {
		t.Fatalf("third run err=%v want ErrOverloaded", err)
	}
	for orch.QueuedRuns() != 1 {
		if ctx.Err() != nil {
			t.Fatal("run never queued")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if run, _ := s.GetRun(ctx, core.DefaultTenant, second); run.Status != core.RunQueued {
		t.Fatalf("queued run status=%s; its MaxRuntime must not run while it waits", run.Status)
	}
	close(release)
	for _, id := range []core.RunID{first, second} {
		if res, err := orch.Wait(ctx, core.DefaultTenant, id); err != nil {
			t.Fatalf("run %s: status=%s err=%v", id, res.Status, err)
		}
	}
	if _, err := start(time.Minute); err != nil {
		t.Fatalf("run after the queue drained: %v", err)
	}
}

func TestCancelQueuedRun(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "gated", Model: "m", Provider: gateProvider(release)}})
	orch.MaxActiveRuns = 1
	ctx, cancel := waitCtx()
	defer cancel()
	wf := runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}}}
	if _, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: wf}); err != nil {
		t.Fatal(err)
	}
	queued, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: wf})
	if err != nil {
		t.Fatal(err)
	}
	for orch.QueuedRuns() != 1 {
		if ctx.Err() != nil {
			t.Fatal("run never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if err := orch.Cancel(ctx, core.DefaultTenant, queued, "", "test"); err != nil {
		t.Fatal(err)
	}
	if res, _ := orch.Wait(ctx, core.DefaultTenant, queued); res.Status != core.RunCanceled {
		t.Fatalf("status=%s want canceled", res.Status)
	}
}

// Acceptance: §Concurrent Sub-Agent Workflow #4 – excess runs wait for their tenant or workflow slot.
func TestTenantAndWorkflowConcurrencyLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits policy.ConcurrencyLimits
		other  func(*runtime.StartRunInput, core.SessionID)
	}{
		{"tenant", policy.ConcurrencyLimits{PerTenant: 1}, func(in *runtime.StartRunInput, sid core.SessionID) {
			in.TenantID, in.SessionID = "other-tenant", sid
		}},
		{"workflow", policy.ConcurrencyLimits{PerWorkflow: 1}, func(in *runtime.StartRunInput, _ core.SessionID) {
			in.Workflow.Name = "other-workflow"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(gateProvider)
			orch, s, sid := setup(t, []runtime.Agent{{Name: "gated", Model: "m", Provider: release}})
			orch.Concurrency = tc.limits
			ctx, cancel := waitCtx()
			defer cancel()
			otherSID, _ := s.CreateSession(ctx, store.Session{TenantID: "other-tenant", CreatedBy: core.DefaultPrincipal})
			in := func() runtime.StartRunInput {
				return runtime.StartRunInput{TenantID: core.DefaultTenant, SessionID: sid, Workflow: runtime.Workflow{
					Name:   "wf",
					Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}},
				}}
			}
			status := func(tenant core.TenantID, id core.RunID) core.RunStatus {
				run, _ := s.GetRun(ctx, tenant, id)
				return run.Status
			}
			awaitRunning := func(tenant core.TenantID, id core.RunID) {
				for status(tenant, id) != core.RunRunning {
					if ctx.Err() != nil {
						t.Fatalf("run %s never started", id)
					}
					time.Sleep(time.Millisecond)
				}
			}
			first, _ := orch.StartRun(ctx, in())
			awaitRunning(core.DefaultTenant, first)
			second, _ := orch.StartRun(ctx, in())
			otherIn := in()
			tc.other(&otherIn, otherSID)
			other, err := orch.StartRun(ctx, otherIn)
			if err != nil {
				t.Fatal(err)
			}
			awaitRunning(otherIn.TenantID, other)
			time.Sleep(20 * time.Millisecond)
			if got := status(core.DefaultTenant, second); got != core.RunQueued {
				t.Fatalf("second run status=%s want queued", got)
			}
			close(release)
			for _, r := range []struct {
				tenant core.TenantID
				id     core.RunID
			}{{core.DefaultTenant, first}, {core.DefaultTenant, second}, {otherIn.TenantID, other}} {
				if res, err := orch.Wait(ctx, r.tenant, r.id); err != nil {
					t.Fatalf("status=%s err=%v", res.Status, err)
				}
			}
		})
	}
}

// Acceptance: §Tool Calling #5 – exclusive tools are serialized across runs; tool groups are bounded.
func TestToolConcurrencyLimits(t *testing.T) {
	var mu sync.Mutex
	active, peak := map[string]int{}, map[string]int{}
	track := func(key string, d time.Duration) {
		mu.Lock()
		active[key]++
		peak[key] = max(peak[key], active[key])
		mu.Unlock()
		time.Sleep(d)
		mu.Lock()
		active[key]--
		mu.Unlock()
	}
	calls := []model.ToolCall{
		{ID: "1", Name: "read", Input: json.RawMessage(`{"n":1}`)},
		{ID: "2", Name: "read", Input: json.RawMessage(`{"n":2}`)},
		{ID: "3", Name: "read", Input: json.RawMessage(`{"n":3}`)},
		{ID: "4", Name: "lock", Input: json.RawMessage(`{}`)},
	}
	var agents []runtime.Agent
	var steps []runtime.AgentStep
	for _, name := range []string{"a", "b", "c"} {
		prov := model.NewMockProvider(model.MockResponse{ToolCalls: calls}, model.MockResponse{Content: "ok"})
		agents = append(agents, runtime.Agent{Name: name, Model: "m", Provider: prov, Tools: []string{"read", "lock"}})
		steps = append(steps, runtime.AgentStep{Name: name, Prompt: "x"})
	}
	orch, _, sid := setup(t, agents)
	orch.Concurrency = policy.ConcurrencyLimits{PerTool: 2}
	_ = orch.Tools.Register(core.ToolDefinition{Name: "read", Concurrency: core.ConcurrencyParallelRead}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		track("read", 20*time.Millisecond)
		return tool.Result{Output: json.RawMessage(`{}`)}, nil
	}))
	_ = orch.Tools.Register(core.ToolDefinition{Name: "lock", Concurrency: core.ConcurrencyExclusive}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		track("lock", 20*time.Millisecond)
		return tool.Result{Output: json.RawMessage(`{}`)}, nil
	}))
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "fan", Kind: runtime.StageParallel, Agents: steps}}},
		Limits:    core.Limits{MaxConcurrent: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatalf("status=%s err=%v", res.Status, err)
	}
	if peak["read"] != 2 || peak["lock"] != 1 {
		t.Fatalf("peak concurrency=%v want read=2 (PerTool) and lock=1 (exclusive)", peak)
	}
}

func TestPerAgentConcurrencyLimit(t *testing.T) {
	prov := &peakProvider{}
	orch, _, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov}})
	orch.Concurrency = policy.ConcurrencyLimits{PerAgent: 1}
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{Name: "fan", Kind: runtime.StageParallel, Agents: []runtime.AgentStep{
			{Name: "a", Prompt: "1"}, {Name: "a", Prompt: "2"}, {Name: "a", Prompt: "3"},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatalf("status=%s err=%v", res.Status, err)
	}
	if prov.peak != 1 {
		t.Fatalf("peak concurrent model calls of agent a=%d want 1", prov.peak)
	}
}

type peakProvider struct {
	mu           sync.Mutex
	active, peak int
}

func (p *peakProvider) Name() string { return "peak" }
func (p *peakProvider) Capabilities(context.Context, string) (model.Capabilities, error) {
	return model.Capabilities{}, nil
}
func (p *peakProvider) Chat(context.Context, model.ChatRequest) (model.ChatResponse, error) {
	p.mu.Lock()
	p.active++
	p.peak = max(p.peak, p.active)
	p.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return model.ChatResponse{Message: model.Message{Role: model.RoleAssistant, Content: "ok"}}, nil
}

func TestToolGroupBoundedByMaxConcurrent(t *testing.T) {
	var mu sync.Mutex
	active, peak := 0, 0
	calls := []model.ToolCall{
		{ID: "1", Name: "read", Input: json.RawMessage(`{"n":1}`)},
		{ID: "2", Name: "read", Input: json.RawMessage(`{"n":2}`)},
		{ID: "3", Name: "read", Input: json.RawMessage(`{"n":3}`)},
	}
	prov := model.NewMockProvider(model.MockResponse{ToolCalls: calls}, model.MockResponse{Content: "ok"})
	orch, _, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov, Tools: []string{"read"}}})
	_ = orch.Tools.Register(core.ToolDefinition{Name: "read", Concurrency: core.ConcurrencyParallelRead}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return tool.Result{Output: json.RawMessage(`{}`)}, nil
	}))
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}}}}},
		Limits:    core.Limits{MaxConcurrent: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatalf("status=%s err=%v", res.Status, err)
	}
	if peak != 2 {
		t.Fatalf("peak parallel tools=%d want 2 (MaxConcurrent)", peak)
	}
}

func TestWaitOnReleasedFailedRunMatchesLiveResult(t *testing.T) {
	prov := model.NewMockProvider(model.MockResponse{Err: errors.New("provider down")})
	orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov}})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	live, liveErr := orch.Wait(ctx, core.DefaultTenant, runID)
	for orch.ActiveRuns() != 0 {
		if ctx.Err() != nil {
			t.Fatal("run never released")
		}
		time.Sleep(time.Millisecond)
	}
	stored, storedErr := orch.Wait(ctx, core.DefaultTenant, runID)
	if live.Status != core.RunFailed || stored.Status != live.Status || stored.FailureReason != live.FailureReason {
		t.Fatalf("live=%+v stored=%+v", live, stored)
	}
	if liveErr == nil || storedErr == nil || liveErr.Error() != storedErr.Error() || !strings.Contains(storedErr.Error(), "provider down") {
		t.Fatalf("liveErr=%v storedErr=%v", liveErr, storedErr)
	}
}

// Acceptance: §Observability and Operations #2 – metrics.
func TestRunMetrics(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{
			{ID: "1", Name: "echo", Input: json.RawMessage(`{"text":"a"}`)},
			{ID: "2", Name: "nope", Input: json.RawMessage(`{}`)},
		}, Usage: core.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, CostUnits: 2}},
		model.MockResponse{Content: "done", Usage: core.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}},
	)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	orch.Metrics.WriteText(&b)
	for _, want := range []string{
		`agentloop_runs_finished_total{status="succeeded"} 1`,
		`agentloop_run_duration_seconds_count 1`,
		`agentloop_model_calls_total{model="m",outcome="ok"} 2`,
		`agentloop_model_call_duration_seconds_count{model="m"} 2`,
		`agentloop_tokens_total{model="m",type="prompt"} 30`,
		`agentloop_tokens_total{model="m",type="completion"} 8`,
		`agentloop_cost_units_total{model="m"} 2`,
		`agentloop_tool_calls_total{tool="echo",outcome="ok"} 1`,
		`agentloop_tool_calls_total{tool="unknown",outcome="error"} 1`,
		`agentloop_tool_call_duration_seconds_count{tool="echo"} 1`,
		"agentloop_runs_queued 0",
		"agentloop_runs_executing 0",
	} {
		if !strings.Contains(b.String(), want+"\n") {
			t.Fatalf("missing %q in:\n%s", want, b.String())
		}
	}
}

// Acceptance: §Session and Run Lifecycle #4 – resume a run after process restart.
func TestRecoverResumesInterruptedRun(t *testing.T) {
	planner := model.NewMockProvider()
	writer := model.NewMockProvider(model.MockResponse{Content: "final"})
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "planner", Model: "m", Provider: planner},
		{Name: "writer", Model: "m", Provider: writer},
	})
	ctx, cancel := waitCtx()
	defer cancel()

	// The state a process leaves behind when it dies during the second stage.
	wf := runtime.Workflow{Name: "wf", Stages: []runtime.Stage{
		{Name: "plan", Agents: []runtime.AgentStep{{Name: "planner", Prompt: "plan {{input.topic}}"}}},
		{Name: "write", Agents: []runtime.AgentStep{{Name: "writer", Prompt: "write from {{stage.plan}}"}}},
	}}
	wfRaw, _ := json.Marshal(wf)
	runID, err := s.CreateRun(ctx, store.Run{
		TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning,
		Limits:   core.Limits{MaxModelCalls: 2}.Defaults(),
		Usage:    core.Usage{ModelCalls: 1, SubAgents: 2, PromptTokens: 7},
		Workflow: wfRaw, Input: json.RawMessage(`{"topic":"go"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, store.Task{
		TenantID: core.DefaultTenant, RunID: runID, AgentName: "planner", Status: core.TaskCompleted,
		Input:  json.RawMessage(`{"prompt":"plan go","stage":"plan"}`),
		Output: json.RawMessage(`{"content":"the plan"}`),
	}); err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.CreateTask(ctx, store.Task{
		TenantID: core.DefaultTenant, RunID: runID, AgentName: "writer", Status: core.TaskRunning,
		Input: json.RawMessage(`{"prompt":"write from the plan","stage":"write"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := orch.Recover(ctx)
	if err != nil || n != 1 {
		t.Fatalf("recovered %d runs, err=%v", n, err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil || res.Outputs["write"] != "final" || res.Outputs["plan"] != "the plan" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if len(planner.Calls()) != 0 {
		t.Fatal("completed planner task ran again")
	}
	if got := writer.Calls()[0].Messages; got[len(got)-1].Content != "write from the plan" {
		t.Fatalf("writer prompt=%q", got[len(got)-1].Content)
	}
	// The interrupted writer attempt stays counted, so the rerun makes it 3 sub-agents.
	if res.Usage.ModelCalls != 2 || res.Usage.PromptTokens < 7 || res.Usage.SubAgents != 3 {
		t.Fatalf("usage not carried over: %+v", res.Usage)
	}
	tasks, _ := s.ListRunTasks(ctx, core.DefaultTenant, runID)
	for _, task := range tasks {
		if task.ID == interrupted && task.Status != core.TaskFailed {
			t.Fatalf("interrupted task status=%s want failed", task.Status)
		}
	}
	if err := orch.Resume(ctx, core.DefaultTenant, runID); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("resume of finished run err=%v want ErrConflict", err)
	}
	events, err := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	if err != nil || len(events) == 0 || !strings.Contains(string(events[0].Payload), `"run_resumed"`) {
		t.Fatalf("first event is not run_resumed: %v err=%v", events, err)
	}
}

// Acceptance: §Observability and Operations #3 – graceful shutdown drains runs.
func TestShutdownDrainsRuns(t *testing.T) {
	release := make(gateProvider)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "gated", Model: "m", Provider: release}})
	ctx, cancel := waitCtx()
	defer cancel()
	in := runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}}}}
	runID, err := orch.StartRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- orch.Shutdown(ctx) }()
	for {
		if _, err := orch.StartRun(ctx, in); errors.Is(err, core.ErrOverloaded) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown returned before the run finished: %v", err)
	default:
	}
	close(release)
	if err := <-shutdown; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("drained run status=%s err=%v", res.Status, err)
	}
}

// Acceptance: §Observability and Operations #3 – runs left at the deadline resume after restart.
func TestShutdownCheckpointsRunsForRecovery(t *testing.T) {
	release := make(gateProvider)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "gated", Model: "m", Provider: release}})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for run, _ := s.GetRun(ctx, core.DefaultTenant, runID); run.Status != core.RunRunning; run, _ = s.GetRun(ctx, core.DefaultTenant, runID) {
		if ctx.Err() != nil {
			t.Fatal("run never started")
		}
		time.Sleep(time.Millisecond)
	}
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if err := orch.Shutdown(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown err=%v want DeadlineExceeded", err)
	}
	run, err := s.GetRun(ctx, core.DefaultTenant, runID)
	if err != nil || run.Status.Terminal() {
		t.Fatalf("checkpointed run=%+v err=%v; want it left unfinished", run, err)
	}
	events, err := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		var p struct{ Status core.RunStatus }
		if ev.Type == core.EvRunStateChanged && json.Unmarshal(ev.Payload, &p) == nil && p.Status.Terminal() {
			t.Fatalf("terminal event written for a checkpointed run: %s", ev.Payload)
		}
	}

	restarted := runtime.NewOrchestrator(s, orch.Tools, []runtime.Agent{{Name: "gated", Model: "m", Provider: release}})
	close(release)
	if n, err := restarted.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("recovered %d err=%v", n, err)
	}
	if res, err := restarted.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("recovered run status=%s err=%v", res.Status, err)
	}
}

func TestShutdownLeavesQueuedRunQueued(t *testing.T) {
	release := make(gateProvider)
	defer close(release)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "gated", Model: "m", Provider: release}})
	orch.MaxActiveRuns = 1
	ctx, cancel := waitCtx()
	defer cancel()
	in := runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}}}}
	first, _ := orch.StartRun(ctx, in)
	for run, _ := s.GetRun(ctx, core.DefaultTenant, first); run.Status != core.RunRunning; run, _ = s.GetRun(ctx, core.DefaultTenant, first) {
		if ctx.Err() != nil {
			t.Fatal("run never started")
		}
		time.Sleep(time.Millisecond)
	}
	queued, err := orch.StartRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if err := orch.Shutdown(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown err=%v", err)
	}
	for id, want := range map[core.RunID]core.RunStatus{first: core.RunRunning, queued: core.RunQueued} {
		if run, err := s.GetRun(ctx, core.DefaultTenant, id); err != nil || run.Status != want {
			t.Fatalf("run status=%s err=%v want %s", run.Status, err, want)
		}
	}
	unfinished, _ := s.ListUnfinishedRuns(ctx)
	if len(unfinished) != 2 {
		t.Fatalf("unfinished runs=%d want 2", len(unfinished))
	}
}

func TestResumeParallelStageAndPendingApproval(t *testing.T) {
	b, c := model.NewMockProvider(), model.NewMockProvider()
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "a", Model: "m", Provider: model.NewMockProvider()},
		{Name: "b", Model: "m", Provider: b},
		{Name: "c", Model: "m", Provider: c},
	})
	orch.MaxActiveRuns, orch.MaxQueuedRuns = 1, 0
	ctx, cancel := waitCtx()
	defer cancel()
	wf := runtime.Workflow{Stages: []runtime.Stage{{Name: "fan", Kind: runtime.StageParallel, FailurePolicy: runtime.WaitAll, Agents: []runtime.AgentStep{
		{Name: "a", Prompt: "x"}, {Name: "b", Prompt: "y"}, {Name: "c", Prompt: "z"},
	}}}}
	wfRaw, _ := json.Marshal(wf)
	var runIDs []core.RunID
	var approvalIDs []core.ApprovalID
	for range 2 {
		// Interrupted mid-stage: all three reserved, a finished, b running, c never started.
		runID, err := s.CreateRun(ctx, store.Run{
			TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunAwaitingApproval,
			Limits: core.Limits{MaxSubAgents: 4}.Defaults(), Usage: core.Usage{SubAgents: 3},
			Workflow: wfRaw,
		})
		if err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, runID)
		for _, task := range []store.Task{
			{AgentName: "a", Status: core.TaskCompleted, Input: json.RawMessage(`{"prompt":"x","stage":"fan"}`), Output: json.RawMessage(`{"content":"A"}`)},
			{AgentName: "b", Status: core.TaskRunning, Input: json.RawMessage(`{"prompt":"y","stage":"fan"}`)},
		} {
			task.TenantID, task.RunID = core.DefaultTenant, runID
			if _, err := s.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
		}
		approvalID, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: runID})
		if err != nil {
			t.Fatal(err)
		}
		approvalIDs = append(approvalIDs, approvalID)
		b.Push(model.MockResponse{Content: "B"})
		c.Push(model.MockResponse{Content: "C"})
	}

	// Both are recovered although only one slot exists and the queue holds none.
	if n, err := orch.Recover(ctx); err != nil || n != 2 {
		t.Fatalf("recovered %d err=%v", n, err)
	}
	for _, runID := range runIDs {
		res, err := orch.Wait(ctx, core.DefaultTenant, runID)
		if err != nil || res.Outputs["fan"] != "a=A\nb=B\nc=C" {
			t.Fatalf("result=%+v err=%v", res, err)
		}
		// a and b ran before; b and c run now. MaxSubAgents 4 is enough only if
		// c's earlier reservation is not counted twice.
		if res.Usage.SubAgents != 4 {
			t.Fatalf("sub-agents=%d want 4", res.Usage.SubAgents)
		}
	}
	for _, id := range approvalIDs {
		if a, err := s.GetApproval(ctx, core.DefaultTenant, id); err != nil || a.Status != "canceled" {
			t.Fatalf("stale approval=%+v err=%v want canceled", a, err)
		}
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
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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

// Acceptance: §Concurrent Sub-Agent Workflow #3 – a quorum stage succeeds with
// the answers of the agents that succeeded when enough of them did, and fails otherwise.
func TestParallelStageQuorum(t *testing.T) {
	for _, tc := range []struct {
		quorum int
		ok     bool
	}{{2, true}, {3, false}} {
		boom := errors.New("provider down")
		orch, _, sid := setup(t, []runtime.Agent{
			{Name: "a", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Content: "A"})},
			{Name: "b", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Err: boom})},
			{Name: "c", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Content: "C"})},
		})
		ctx, cancel := waitCtx()
		runID, err := orch.StartRun(ctx, runtime.StartRunInput{
			SessionID: sid,
			Workflow: runtime.Workflow{Stages: []runtime.Stage{{
				Name: "fan", Kind: runtime.StageParallel, FailurePolicy: runtime.Quorum, Quorum: tc.quorum,
				Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}, {Name: "b", Prompt: "y"}, {Name: "c", Prompt: "z"}},
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		res, err := orch.Wait(ctx, core.DefaultTenant, runID)
		cancel()
		if tc.ok {
			if err != nil || res.Outputs["fan"] != "a=A\nc=C" {
				t.Fatalf("quorum %d: err=%v outputs=%q", tc.quorum, err, res.Outputs)
			}
			continue
		}
		if res.Status != core.RunFailed || !strings.Contains(res.FailureReason, "1 of 3 agents failed, so fewer than the quorum of 3 can succeed") || !strings.Contains(res.FailureReason, "provider down") {
			t.Fatalf("quorum %d: status=%s reason=%s", tc.quorum, res.Status, res.FailureReason)
		}
	}
}

// fail_fast and an unreachable quorum cancel the agents still running.
func TestParallelStageFailureCancelsSiblings(t *testing.T) {
	for _, policy := range []runtime.Stage{{FailurePolicy: runtime.FailFast}, {FailurePolicy: runtime.Quorum, Quorum: 2}} {
		orch, s, sid := setup(t, []runtime.Agent{
			{Name: "fails", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Err: errors.New("provider down")})},
			{Name: "blocks", Model: "m", Provider: gateProvider(make(chan struct{}))},
		})
		ctx, cancel := waitCtx()
		stage := policy
		stage.Name, stage.Kind = "fan", runtime.StageParallel
		stage.Agents = []runtime.AgentStep{{Name: "fails", Prompt: "x"}, {Name: "blocks", Prompt: "y"}}
		runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{stage}}})
		if err != nil {
			t.Fatal(err)
		}
		res, _ := orch.Wait(ctx, core.DefaultTenant, runID)
		cancel()
		if res.Status != core.RunFailed || !strings.Contains(res.FailureReason, "provider down") {
			t.Fatalf("%s: status=%s reason=%s", policy.FailurePolicy, res.Status, res.FailureReason)
		}
		events, _ := s.ListRunEvents(context.Background(), core.DefaultTenant, runID, 0, 0)
		var agents []string
		for _, ev := range events {
			if ev.Type == core.EvError {
				var p struct{ Agent, Scope string }
				_ = json.Unmarshal(ev.Payload, &p)
				agents = append(agents, p.Scope+":"+p.Agent)
			}
		}
		if !slices.Equal(agents, []string{"task:fails", "run:"}) {
			t.Fatalf("%s: canceled sibling recorded an error: %v", policy.FailurePolicy, agents)
		}
	}
}

// best_effort and wait_all wait for every agent: best_effort keeps the answers
// that succeeded, wait_all fails on any failure.
func TestParallelStageBestEffortAndWaitAllWithAFailure(t *testing.T) {
	for policy, ok := range map[runtime.FailurePolicy]bool{runtime.BestEffort: true, runtime.WaitAll: false} {
		orch, _, sid := setup(t, []runtime.Agent{
			{Name: "fails", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Err: errors.New("provider down")})},
			{Name: "slow", Model: "m", Provider: &slowProvider{delay: 50 * time.Millisecond}},
		})
		ctx, cancel := waitCtx()
		runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name: "fan", Kind: runtime.StageParallel, FailurePolicy: policy,
			Agents: []runtime.AgentStep{{Name: "fails", Prompt: "x"}, {Name: "slow", Prompt: "y"}},
		}}}})
		if err != nil {
			t.Fatal(err)
		}
		res, _ := orch.Wait(ctx, core.DefaultTenant, runID)
		cancel()
		if ok && (res.Status != core.RunSucceeded || res.Outputs["fan"] != "slow=slow") ||
			!ok && (res.Status != core.RunFailed || !strings.Contains(res.FailureReason, "provider down")) {
			t.Fatalf("%s: status=%s outputs=%q reason=%s", policy, res.Status, res.Outputs, res.FailureReason)
		}
	}
}

// A canceled run whose quorum stage already met its quorum ends canceled, not succeeded.
func TestQuorumStageMetThenRunCanceled(t *testing.T) {
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "a", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Content: "A"})},
		{Name: "b", Model: "m", Provider: gateProvider(make(chan struct{}))},
	})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{
		Name: "fan", Kind: runtime.StageParallel, FailurePolicy: runtime.Quorum, Quorum: 1,
		Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}, {Name: "b", Prompt: "y"}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	for done := false; !done; time.Sleep(5 * time.Millisecond) {
		events, _ := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
		for _, ev := range events {
			done = done || ev.Type == core.EvTaskCompleted
		}
	}
	if err := orch.Cancel(ctx, core.DefaultTenant, runID, "", "test"); err != nil {
		t.Fatal(err)
	}
	if res, _ := orch.Wait(ctx, core.DefaultTenant, runID); res.Status != core.RunCanceled {
		t.Fatalf("status=%s outputs=%q", res.Status, res.Outputs)
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
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Status != core.RunSucceeded {
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
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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

// Acceptance: §Tool Calling #1 – input is validated against the tool's schema
// before approval and execution, and a mismatch goes back to the model.
func TestToolInputValidatedBeforeExecution(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "refund", Input: json.RawMessage(`{"order_id":7}`)}}},
		model.MockResponse{Content: "fixed"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"refund"}}})
	ran := false
	if err := orch.Tools.Register(core.ToolDefinition{
		Name:             "refund",
		RequiresApproval: true,
		InputSchema:      json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`),
	}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		ran = true
		return tool.Result{Output: json.RawMessage(`{}`)}, nil
	})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil || res.Outputs["s"] != "fixed" {
		t.Fatalf("status=%s err=%v reason=%s", res.Status, err, res.FailureReason)
	}
	if ran {
		t.Fatal("tool ran with input that does not match its schema")
	}
	msgs := prov.Calls()[1].Messages
	if got := msgs[len(msgs)-1].Content; !strings.Contains(got, "invalid input for tool refund") {
		t.Fatalf("validation error not returned to the model: %s", got)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	for _, ev := range events {
		if ev.Type == core.EvApprovalReq || ev.Type == core.EvToolStarted {
			t.Fatalf("invalid call reached %s", ev.Type)
		}
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
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
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
	if err := orch.Cancel(ctx, core.DefaultTenant, runID, "", "test"); err != nil {
		t.Fatal(err)
	}
	res, _ := orch.Wait(ctx, core.DefaultTenant, runID)
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

	approvalID := awaitApprovalRequest(t, s, runID)
	run, err := s.GetRun(ctx, core.DefaultTenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != core.RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, approvalID, "approved", "approver-1", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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
			if ev.ActorID != "approver-1" || !strings.Contains(string(ev.Payload), `"resolved_by":"approver-1"`) {
				t.Fatalf("approval_resolved does not name its resolver: actor=%q %s", ev.ActorID, ev.Payload)
			}
		}
	}
	if !foundResolution {
		t.Fatal("approval resolution event was not emitted")
	}
	byID := map[core.EventID]core.Event{}
	var chain []string
	for _, ev := range events {
		byID[ev.ID] = ev
		if ev.Type == core.EvToolCompleted {
			for cur := ev; ; cur = byID[core.EventID(cur.CausationID)] {
				chain = append([]string{cur.Type}, chain...)
				if cur.CausationID == "" {
					break
				}
			}
		}
	}
	want := []string{core.EvToolScheduled, core.EvApprovalReq, core.EvApprovalRes, core.EvToolStarted, core.EvToolCompleted}
	if !slices.Equal(chain, want) {
		t.Fatalf("causation chain=%v want %v", chain, want)
	}
}

// Acceptance: §Tool Calling #3 – approval for every external side-effecting
// tool, by class; re-execution does not ask again.
func TestSideEffectPolicyRequiresApproval(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "notify", Input: json.RawMessage(`{}`)}, {ID: "2", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}}},
		model.MockResponse{Content: "sent"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"notify", "echo"}}})
	orch.Policy.ApproveSideEffects = []core.SideEffectLevel{core.SideWriteExternal}
	if err := orch.Tools.Register(core.ToolDefinition{Name: "notify", SideEffect: core.SideWriteExternal}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{Output: json.RawMessage(`{"sent":true}`)}, nil
	})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	approvalID := awaitApprovalRequest(t, s, runID)
	if err := s.ResolveApproval(ctx, core.DefaultTenant, approvalID, "approved", "", nil); err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Outputs["s"] != "sent" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	requested, _ := approvalEvents(t, s, runID)
	if requested["tool"] != "notify" || !strings.Contains(requested["reason"].(string), "write_external") {
		t.Fatalf("approval_requested=%v", requested)
	}
	if list, _ := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{RunID: runID}); len(list) != 1 {
		t.Fatalf("approvals=%d want 1 (echo has no side effect)", len(list))
	}
	again, err := orch.ReExecute(ctx, core.DefaultTenant, runID)
	if err != nil || again.Outputs["s"] != "sent" {
		t.Fatalf("re-execute: %+v %v", again, err)
	}
}

// A workflow's tool policy narrows what its runs may call, on top of the server's.
func TestWorkflowToolPolicy(t *testing.T) {
	call := model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}}}
	prov := model.NewMockProvider(call, model.MockResponse{Content: "done"}, call, model.MockResponse{Content: "done"})
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
	ctx, cancel := waitCtx()
	defer cancel()
	run := func(p *policy.ToolPolicy) (core.RunID, []model.ToolSpec) {
		t.Helper()
		runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{
			ToolPolicy: p,
			Stages:     []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		calls := prov.Calls()
		return runID, calls[len(calls)-2].Tools
	}
	denied, tools := run(&policy.ToolPolicy{DenySideEffects: []core.SideEffectLevel{core.SideNone}})
	if len(tools) != 0 {
		t.Fatalf("denied tool offered: %v", tools)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, denied, 0, 0)
	for _, ev := range events {
		if ev.Type == core.EvToolCompleted && !strings.Contains(string(ev.Payload), "not available") {
			t.Fatalf("denied tool ran: %s", ev.Payload)
		}
	}
	if _, tools := run(nil); len(tools) != 1 {
		t.Fatalf("a run without the policy lost the tool: %v", tools)
	}
	orch.Workflows["t"] = runtime.Workflow{Name: "t", Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}}
	if _, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Template: "t", Workflow: runtime.Workflow{
		ToolPolicy: &policy.ToolPolicy{Denylist: []string{"echo"}},
	}}); err == nil {
		t.Fatal("a template run dropped the policy sent with it instead of refusing it")
	}
	prov.Push(call)
	prov.Push(model.MockResponse{Content: "done"})
	gated, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{
		ToolPolicy: &policy.ToolPolicy{ApproveSideEffects: []core.SideEffectLevel{core.SideNone}},
		Stages:     []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, awaitApprovalRequest(t, s, gated), "approved", "", nil); err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, gated); err != nil || res.Outputs["s"] != "done" {
		t.Fatalf("approved run: %+v %v", res, err)
	}
	if _, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{
		ToolPolicy: &policy.ToolPolicy{ApproveSideEffects: []core.SideEffectLevel{"emails"}},
		Stages:     []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}},
	}}); err == nil {
		t.Fatal("unknown side effect accepted")
	}
}

// An agent saves an artifact through a tool; it is stored with its run, task,
// step and producer, and announced with artifact_created.
func TestToolArtifactsAreStoredAndAnnounced(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "artifact.save", Input: json.RawMessage(`{"name":"report.md","content":"# Findings","kind":"report","mime_type":"text/markdown"}`)}}},
		model.MockResponse{Content: "saved"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "writer", Model: "m", Provider: prov, Tools: []string{"artifact.save"}}})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "writer", Prompt: "x"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	artifacts, _ := s.ListRunArtifacts(ctx, core.DefaultTenant, runID)
	tasks, _ := s.ListRunTasks(ctx, core.DefaultTenant, runID)
	if len(artifacts) != 1 || len(tasks) != 1 {
		t.Fatalf("artifacts=%+v tasks=%d", artifacts, len(tasks))
	}
	a := artifacts[0]
	if a.Name != "report.md" || a.Kind != "report" || a.MimeType != "text/markdown" || string(a.Content) != "# Findings" ||
		a.Producer != "artifact.save" || a.TaskID != tasks[0].ID || a.StepID == "" || a.SessionID != sid {
		t.Fatalf("artifact=%+v", a)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	var announced, completed bool
	for _, ev := range events {
		switch ev.Type {
		case core.EvArtifact:
			announced = ev.StepID == a.StepID && ev.Visibility == core.VisibilityParticipant && strings.Contains(string(ev.Payload), a.ID)
		case core.EvToolCompleted:
			completed = strings.Contains(string(ev.Payload), `"artifacts":["`+a.ID+`"]`)
		}
	}
	if !announced || !completed {
		t.Fatalf("artifact_created=%v tool_call_completed lists it=%v", announced, completed)
	}
}

func awaitApprovalRequest(t *testing.T, s store.Store, runID core.RunID) core.ApprovalID {
	t.Helper()
	ctx := context.Background()
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
			return payload.ApprovalID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("approval request was not emitted")
	return ""
}

// Acceptance: §Human Approval #3 – a rejection goes back to the agent.
func TestRejectedApprovalReturnsToAgent(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}}},
		model.MockResponse{Content: "ok, skipped"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
	orch.Policy.ApprovalRequired = []string{"echo"}
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	approvalID := awaitApprovalRequest(t, s, runID)
	if err := s.ResolveApproval(ctx, core.DefaultTenant, approvalID, "rejected", "", nil); err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil || res.Outputs["s"] != "ok, skipped" {
		t.Fatalf("status=%s err=%v reason=%s", res.Status, err, res.FailureReason)
	}
	msgs := prov.Calls()[1].Messages
	if last := msgs[len(msgs)-1]; last.Role != model.RoleTool || !strings.Contains(last.Content, "rejected") {
		t.Fatalf("agent did not get the rejection: %+v", last)
	}
	events, err := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for _, ev := range events {
		if ev.Type == core.EvToolStarted {
			t.Fatal("rejected tool was executed")
		}
		if ev.Type == core.EvRunStateChanged {
			var p struct{ Status string }
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			statuses = append(statuses, p.Status)
		}
	}
	if got := strings.Join(statuses, ","); got != "running,awaiting_approval,running,succeeded" {
		t.Fatalf("run states=%s", got)
	}
}

// Acceptance: §Human Approval #4 – an unanswered approval expires.
func TestApprovalExpires(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}}},
		model.MockResponse{Content: "gave up"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
	orch.Policy.ApprovalRequired = []string{"echo"}
	orch.ApprovalTimeout = 50 * time.Millisecond
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	approvalID := awaitApprovalRequest(t, s, runID)
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil || res.Outputs["s"] != "gave up" {
		t.Fatalf("status=%s err=%v reason=%s", res.Status, err, res.FailureReason)
	}
	msgs := prov.Calls()[1].Messages
	if last := msgs[len(msgs)-1]; !strings.Contains(last.Content, "expired") {
		t.Fatalf("agent was not told the approval expired: %+v", last)
	}
	a, err := s.GetApproval(ctx, core.DefaultTenant, approvalID)
	if err != nil || a.Status != "expired" {
		t.Fatalf("approval=%+v err=%v", a, err)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, approvalID, "approved", "", nil); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("late approval err=%v want ErrConflict", err)
	}
	requested, resolved := approvalEvents(t, s, runID)
	if requested["expires_at"] == nil || resolved["status"] != "expired" {
		t.Fatalf("requested=%v resolved=%v", requested, resolved)
	}
}

func approvalEvents(t *testing.T, s store.Store, runID core.RunID) (requested, resolved map[string]any) {
	t.Helper()
	events, err := s.ListRunEvents(context.Background(), core.DefaultTenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		var p map[string]any
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case core.EvApprovalReq:
			requested = p
		case core.EvApprovalRes:
			resolved = p
		}
	}
	return requested, resolved
}

func TestCanceledRunCancelsPendingApproval(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}}},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
	orch.Policy.ApprovalRequired = []string{"echo"}
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	approvalID := awaitApprovalRequest(t, s, runID)
	if err := orch.Cancel(ctx, core.DefaultTenant, runID, "operator-7", "wrong order"); err != nil {
		t.Fatal(err)
	}
	if res, _ := orch.Wait(ctx, core.DefaultTenant, runID); res.Status != core.RunCanceled {
		t.Fatalf("status=%s want canceled", res.Status)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	audited := false
	for _, ev := range events {
		if ev.Type == core.EvSystem && ev.Visibility == core.VisibilityAudit && ev.ActorID == "operator-7" &&
			strings.Contains(string(ev.Payload), `"event":"run_cancel_requested"`) && strings.Contains(string(ev.Payload), `"reason":"wrong order"`) {
			audited = true
		}
	}
	if !audited {
		t.Fatal("cancellation not audited with its principal and reason")
	}
	a, err := s.GetApproval(ctx, core.DefaultTenant, approvalID)
	if err != nil || a.Status != "canceled" {
		t.Fatalf("approval=%+v err=%v", a, err)
	}
	if _, resolved := approvalEvents(t, s, runID); resolved["status"] != "canceled" {
		t.Fatalf("resolved=%v", resolved)
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
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
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
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
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
	_, err = orch.Wait(ctx, core.DefaultTenant, runID)
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
		if err := orch.Cancel(context.Background(), core.DefaultTenant, runID, "", "test"); err != nil {
			t.Error(err)
		}
	}()
	res, _ := orch.Wait(ctx, core.DefaultTenant, runID)
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
	if err := orch.Cancel(ctx, "other-tenant", runID, "", "test"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cross-tenant cancel err=%v want ErrNotFound", err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("status=%s err=%v want succeeded", res.Status, err)
	}
}

// Acceptance: §Budget and Safety #5 – long idle wait emits a diagnostic event.
func TestIdleWaitEmitsDiagnostic(t *testing.T) {
	orch, s, sid := setup(t, []runtime.Agent{{Name: "slow", Model: "m", Provider: &slowProvider{delay: 300 * time.Millisecond}}})
	orch.IdleWarnAfter = 50 * time.Millisecond
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "slow", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatalf("status=%s err=%v", res.Status, err)
	}
	events, err := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var idleWaits int
	for i, ev := range events {
		var p struct {
			Event     string
			LastEvent string `json:"last_event"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if ev.Type == core.EvSystem && p.Event == "idle_wait" {
			idleWaits++
			if p.LastEvent != core.EvModelStarted {
				t.Fatalf("idle_wait last_event=%q want %s", p.LastEvent, core.EvModelStarted)
			}
		}
		if i == len(events)-1 && ev.Type != core.EvRunStateChanged {
			t.Fatalf("last event is %s, want the terminal run_state_changed", ev.Type)
		}
	}
	if idleWaits == 0 {
		t.Fatal("no idle_wait event")
	}
}

// gateProvider answers "done" once its channel is closed.
type gateProvider chan struct{}

func (gateProvider) Name() string { return "gate" }
func (gateProvider) Capabilities(context.Context, string) (model.Capabilities, error) {
	return model.Capabilities{}, nil
}
func (g gateProvider) Chat(ctx context.Context, _ model.ChatRequest) (model.ChatResponse, error) {
	select {
	case <-ctx.Done():
		return model.ChatResponse{}, ctx.Err()
	case <-g:
		return model.ChatResponse{Message: model.Message{Role: model.RoleAssistant, Content: "done"}}, nil
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
