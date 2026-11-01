package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

func runWorkflow(t *testing.T, orch *runtime.Orchestrator, in runtime.StartRunInput) (runtime.Result, error) {
	t.Helper()
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return orch.Wait(ctx, core.DefaultTenant, runID)
}

func systemEvents(t *testing.T, s store.Store, runID core.RunID, name string) []map[string]any {
	t.Helper()
	events, err := s.ListRunEvents(context.Background(), core.DefaultTenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range events {
		var p map[string]any
		if ev.Type == core.EvSystem && json.Unmarshal(ev.Payload, &p) == nil && p["event"] == name {
			out = append(out, p)
		}
	}
	return out
}

func lastPrompt(req model.ChatRequest) string { return req.Messages[len(req.Messages)-1].Content }

func TestGraphStagesRunWhenDependenciesFinish(t *testing.T) {
	branches := &peakProvider{}
	joiner := model.NewMockProvider(model.MockResponse{Content: "joined"})
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "branch", Model: "m", Provider: branches},
		{Name: "joiner", Model: "m", Provider: joiner},
	})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "join", DependsOn: []string{"left", "right"}, Agents: []runtime.AgentStep{{Name: "joiner", Prompt: "{{stage.left}}+{{stage.right}}"}}},
		{Name: "left", Agents: []runtime.AgentStep{{Name: "branch", Prompt: "l"}}},
		{Name: "right", Agents: []runtime.AgentStep{{Name: "branch", Prompt: "r"}}},
	}}})
	if err != nil || res.Outputs["join"] != "joined" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if branches.peak != 2 {
		t.Fatalf("independent stages ran %d at a time, want 2", branches.peak)
	}
	if got := lastPrompt(joiner.Calls()[0]); got != "ok+ok" {
		t.Fatalf("join prompt=%q, want both branch outputs", got)
	}
}

// Acceptance: §Sequential Multi-Agent Workflow #1 and #2 – review hands work back to code.
func TestHandBackRerunsEarlierStage(t *testing.T) {
	coder := model.NewMockProvider(model.MockResponse{Content: "v1"}, model.MockResponse{Content: "v2"})
	reviewer := model.NewMockProvider(model.MockResponse{Content: "REJECTED: add tests"}, model.MockResponse{Content: "APPROVED"})
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "analyst", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Content: "spec"})},
		{Name: "coder", Model: "m", Provider: coder},
		{Name: "reviewer", Model: "m", Provider: reviewer},
		{Name: "tester", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Content: "pass"})},
	})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "requirements", Agents: []runtime.AgentStep{{Name: "analyst", Prompt: "write a spec"}}},
		{Name: "code", Agents: []runtime.AgentStep{{Name: "coder", Prompt: "implement {{stage.requirements}}; feedback: {{feedback}}"}}},
		{Name: "review", HandBack: &runtime.HandBack{When: "^REJECTED", To: "code"}, Agents: []runtime.AgentStep{{Name: "reviewer", Prompt: "review {{stage.code}}"}}},
		{Name: "test", Agents: []runtime.AgentStep{{Name: "tester", Prompt: "test {{stage.code}}"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if res.Outputs["code"] != "v2" || res.Outputs["review"] != "APPROVED" || res.Outputs["test"] != "pass" {
		t.Fatalf("outputs=%v", res.Outputs)
	}
	calls := coder.Calls()
	if len(calls) != 2 || lastPrompt(calls[0]) != "implement spec; feedback: " || lastPrompt(calls[1]) != "implement spec; feedback: REJECTED: add tests" {
		t.Fatalf("coder prompts: %v", calls)
	}
	if lastPrompt(reviewer.Calls()[1]) != "review v2" {
		t.Fatalf("second review saw %q", lastPrompt(reviewer.Calls()[1]))
	}
	if got := systemEvents(t, s, runID, "stage_handed_back"); len(got) != 1 || got[0]["to"] != "code" {
		t.Fatalf("stage_handed_back events=%v", got)
	}
}

// Acceptance: §Sequential Multi-Agent Workflow #3 – the hand-back limit fails the run.
func TestHandBackLimitFailsRun(t *testing.T) {
	rejections := []model.MockResponse{{Content: "REJECTED"}, {Content: "REJECTED"}, {Content: "REJECTED"}}
	coder := model.NewMockProvider(model.MockResponse{Content: "a"}, model.MockResponse{Content: "b"}, model.MockResponse{Content: "c"})
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "coder", Model: "m", Provider: coder},
		{Name: "reviewer", Model: "m", Provider: model.NewMockProvider(rejections...)},
	})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "code", Agents: []runtime.AgentStep{{Name: "coder", Prompt: "code"}}},
		{Name: "review", HandBack: &runtime.HandBack{When: "REJECTED", To: "code", MaxTimes: 2}, Agents: []runtime.AgentStep{{Name: "reviewer", Prompt: "review"}}},
	}}})
	if err == nil || res.Status != core.RunFailed || !strings.Contains(res.FailureReason, `handing work back to "code" 2 times`) {
		t.Fatalf("status=%s reason=%q err=%v", res.Status, res.FailureReason, err)
	}
	if n := len(coder.Calls()); n != 3 {
		t.Fatalf("code ran %d times, want 3", n)
	}
}

func TestStopWhenEndsRunEarly(t *testing.T) {
	worker := model.NewMockProvider()
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "triage", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Content: "NOTHING TO DO"})},
		{Name: "worker", Model: "m", Provider: worker},
	})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "triage", StopWhen: "^NOTHING TO DO", Agents: []runtime.AgentStep{{Name: "triage", Prompt: "triage"}}},
		{Name: "work", Agents: []runtime.AgentStep{{Name: "worker", Prompt: "work"}}},
	}}})
	if err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if _, ran := res.Outputs["work"]; ran || len(worker.Calls()) != 0 {
		t.Fatal("stage after the stop condition ran")
	}
	if got := systemEvents(t, s, res.RunID, "workflow_stopped"); len(got) != 1 {
		t.Fatalf("workflow_stopped events=%v", got)
	}
}

func TestStageRetryPolicy(t *testing.T) {
	prov := model.NewMockProvider(model.MockResponse{Err: errors.New("flaky")}, model.MockResponse{Content: "ok"})
	orch, s, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov}})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Retry: runtime.RetryPolicy{MaxAttempts: 2, Backoff: time.Millisecond}, Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}}},
	}}})
	if err != nil || res.Outputs["s"] != "ok" || res.Usage.SubAgents != 2 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	tasks, err := s.ListRunTasks(context.Background(), core.DefaultTenant, res.RunID)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks=%v err=%v", tasks, err)
	}
	if tasks[0].Attempt != 1 || tasks[0].Status != core.TaskFailed || tasks[1].Attempt != 2 || tasks[1].Status != core.TaskCompleted {
		t.Fatalf("attempts: %+v", tasks)
	}
	if got := systemEvents(t, s, res.RunID, "task_retry_scheduled"); len(got) != 1 {
		t.Fatalf("task_retry_scheduled events=%v", got)
	}
	var metrics strings.Builder
	orch.Metrics.WriteText(&metrics)
	if !strings.Contains(metrics.String(), `agentloop_task_retries_total{agent="a"} 1`) {
		t.Fatalf("retry metric missing:\n%s", metrics.String())
	}
}

// errorEvents returns the run's error_recorded payloads, checking each one's
// visibility and that the run-scope one comes just before the terminal event.
func errorEvents(t *testing.T, s store.Store, runID core.RunID) []map[string]any {
	t.Helper()
	events, err := s.ListRunEvents(context.Background(), core.DefaultTenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for i, ev := range events {
		if ev.Type != core.EvError {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(ev.Payload, &p)
		want := core.VisibilityInternal
		if p["scope"] == "run" {
			want = core.VisibilityParticipant
			if i != len(events)-2 || events[i+1].Type != core.EvRunStateChanged {
				t.Fatalf("run error_recorded is not just before the terminal event")
			}
		}
		if ev.Visibility != want {
			t.Fatalf("%v: visibility=%s", p, ev.Visibility)
		}
		out = append(out, p)
	}
	return out
}

func TestFailuresRecordErrorEvents(t *testing.T) {
	retried := runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Retry: runtime.RetryPolicy{MaxAttempts: 2, Backoff: time.Millisecond}, Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}}},
	}}
	prov := model.NewMockProvider(model.MockResponse{Err: errors.New("flaky")}, model.MockResponse{Content: "ok"})
	orch, s, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov}})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: retried})
	if err != nil {
		t.Fatal(err)
	}
	got := errorEvents(t, s, res.RunID)
	if len(got) != 1 || got[0]["scope"] != "task" || got[0]["code"] != "provider_unavailable" || got[0]["will_retry"] != true ||
		got[0]["agent"] != "a" || got[0]["attempt"] != 1.0 || !strings.Contains(got[0]["message"].(string), "flaky") {
		t.Fatalf("retried failure: %v", got)
	}

	// The sub-agent budget leaves no room for the retry.
	prov = model.NewMockProvider(model.MockResponse{Err: errors.New("flaky")})
	orch, s, sid = setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov}})
	res, _ = runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Limits: core.Limits{MaxSubAgents: 1}, Workflow: retried})
	got = errorEvents(t, s, res.RunID)
	if res.Status != core.RunFailed || len(got) != 2 || got[0]["will_retry"] != false || got[0]["code"] != "budget_exceeded" ||
		got[1]["scope"] != "run" || got[1]["code"] != "budget_exceeded" {
		t.Fatalf("status=%s errors=%v", res.Status, got)
	}

	orch, s, sid = setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: &slowProvider{delay: time.Second}}})
	res, _ = runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Timeout: 20 * time.Millisecond, Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}}},
	}}})
	got = errorEvents(t, s, res.RunID)
	if res.Status != core.RunTimedOut || len(got) != 1 || got[0]["scope"] != "run" || got[0]["code"] != "timed_out" {
		t.Fatalf("stage timeout: status=%s errors=%v", res.Status, got)
	}
}

func TestStageAndAgentTimeouts(t *testing.T) {
	for name, tc := range map[string]struct {
		stageTimeout, agentTimeout time.Duration
		reason                     string
	}{
		"stage": {stageTimeout: 30 * time.Millisecond, reason: `stage "s" timed out after 30ms`},
		"agent": {agentTimeout: 30 * time.Millisecond, reason: "agent slow timed out after 30ms"},
	} {
		t.Run(name, func(t *testing.T) {
			orch, _, sid := setup(t, []runtime.Agent{{Name: "slow", Model: "m", Provider: &slowProvider{delay: 5 * time.Second}, Timeout: tc.agentTimeout}})
			res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
				{Name: "s", Timeout: tc.stageTimeout, Agents: []runtime.AgentStep{{Name: "slow", Prompt: "x"}}},
			}}})
			if !errors.Is(err, context.DeadlineExceeded) || res.Status != core.RunTimedOut || !strings.Contains(res.FailureReason, tc.reason) {
				t.Fatalf("status=%s reason=%q err=%v", res.Status, res.FailureReason, err)
			}
		})
	}
}

func TestToolTimeoutReturnsErrorToModel(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "hang", Input: json.RawMessage(`{}`)}}},
		model.MockResponse{Content: "recovered"},
	)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov, Tools: []string{"hang"}}})
	if err := orch.Tools.Register(core.ToolDefinition{Name: "hang", Timeout: 20 * time.Millisecond}, tool.ExecutorFunc(func(ctx context.Context, _ tool.Call) (tool.Result, error) {
		<-ctx.Done()
		return tool.Result{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}}},
	}}})
	if err != nil || res.Outputs["s"] != "recovered" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if got := lastPrompt(prov.Calls()[1]); !strings.Contains(got, "deadline exceeded") {
		t.Fatalf("tool result=%q, want the timeout error", got)
	}
}

// awaitGate returns the open approval for a stage gate.
func awaitGate(t *testing.T, s store.Store, runID core.RunID, gate string) core.ApprovalID {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := s.ListRunEvents(context.Background(), core.DefaultTenant, runID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			var p struct {
				ApprovalID core.ApprovalID `json:"approval_id"`
				Gate       string          `json:"gate"`
			}
			if ev.Type == core.EvApprovalReq && json.Unmarshal(ev.Payload, &p) == nil && p.Gate == gate {
				return p.ApprovalID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no approval requested for the %s gate", gate)
	return ""
}

func TestStageApprovalGates(t *testing.T) {
	prov := model.NewMockProvider(model.MockResponse{Content: "deployed"})
	orch, s, sid := setup(t, []runtime.Agent{{Name: "deployer", Model: "m", Provider: prov}})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "deploy", ApproveStart: true, ApproveOutput: true, Agents: []runtime.AgentStep{{Name: "deployer", Prompt: "deploy"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	start := awaitGate(t, s, runID, "start")
	if len(prov.Calls()) != 0 {
		t.Fatal("stage started before its start gate was approved")
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, start, "approved", "", nil); err != nil {
		t.Fatal(err)
	}
	output := awaitGate(t, s, runID, "output")
	if err := s.ResolveApproval(ctx, core.DefaultTenant, output, "rejected", "", nil); err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err == nil || res.Status != core.RunFailed || !strings.Contains(res.FailureReason, "output was not approved (rejected)") {
		t.Fatalf("status=%s reason=%q err=%v", res.Status, res.FailureReason, err)
	}
	approval, err := s.GetApproval(ctx, core.DefaultTenant, output)
	if err != nil || !strings.Contains(string(approval.Request), `"output":"deployed"`) {
		t.Fatalf("output gate request=%s err=%v", approval.Request, err)
	}
}

func delegateCall(id, agent, task string) model.ToolCall {
	raw, _ := json.Marshal(map[string]string{"agent": agent, "task": task})
	return model.ToolCall{ID: id, Name: "delegate", Input: raw}
}

func TestDelegateRunsChildTask(t *testing.T) {
	lead := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{delegateCall("1", "researcher", "find X")}},
		model.MockResponse{Content: "summary"},
	)
	researcher := model.NewMockProvider(model.MockResponse{Content: "X is 42"})
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "lead", Model: "m", Provider: lead, Children: []string{"researcher"}},
		{Name: "researcher", Role: "finds facts", Model: "m", Provider: researcher},
	})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "lead", Prompt: "answer"}}},
	}}})
	if err != nil || res.Outputs["s"] != "summary" || res.Usage.SubAgents != 2 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if tools := lead.Calls()[0].Tools; len(tools) != 1 || tools[0].Name != "delegate" || !strings.Contains(tools[0].Description, "researcher: finds facts") {
		t.Fatalf("lead tools=%+v", tools)
	}
	if lastPrompt(researcher.Calls()[0]) != "find X" || !strings.Contains(lastPrompt(lead.Calls()[1]), "X is 42") {
		t.Fatal("the child's task or answer did not pass between the agents")
	}
	tasks, err := s.ListRunTasks(context.Background(), core.DefaultTenant, res.RunID)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	parent, child := tasks[0], tasks[1]
	if child.AgentName != "researcher" || child.ParentTaskID != parent.ID || child.Depth != 1 || parent.Depth != 0 {
		t.Fatalf("parent=%+v child=%+v", parent, child)
	}
}

func TestEventsShareCorrelationAndLinkParentAndCause(t *testing.T) {
	lead := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{delegateCall("1", "researcher", "find X")}},
		model.MockResponse{Content: "summary"},
	)
	researcher := model.NewMockProvider(model.MockResponse{Content: "X is 42"})
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "lead", Model: "m", Provider: lead, Children: []string{"researcher"}},
		{Name: "researcher", Model: "m", Provider: researcher},
	})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "lead", Prompt: "answer"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	events, _ := s.ListRunEvents(context.Background(), core.DefaultTenant, res.RunID, 0, 0)
	byID := map[core.EventID]core.Event{}
	spawned := map[core.TaskID]core.Event{}
	for _, ev := range events {
		byID[ev.ID] = ev
		if ev.Type == core.EvTaskSpawned {
			spawned[ev.TaskID] = ev
		}
	}
	tasks, _ := s.ListRunTasks(context.Background(), core.DefaultTenant, res.RunID)
	parent, child := tasks[0], tasks[1]
	if spawned[child.ID].ParentEventID != spawned[parent.ID].ID || spawned[parent.ID].ParentEventID != "" {
		t.Fatalf("child task_spawned parent=%s, want the parent task's %s", spawned[child.ID].ParentEventID, spawned[parent.ID].ID)
	}
	causes := map[string]string{}
	for _, ev := range events {
		if ev.CorrelationID != string(res.RunID) {
			t.Fatalf("%s correlation=%s, want the run ID", ev.Type, ev.CorrelationID)
		}
		if ev.TaskID != "" && ev.Type != core.EvTaskSpawned && ev.ParentEventID != spawned[ev.TaskID].ID {
			t.Fatalf("%s parent=%s, want its task's task_spawned", ev.Type, ev.ParentEventID)
		}
		if ev.CausationID != "" {
			cause := byID[core.EventID(ev.CausationID)]
			if cause.StepID != ev.StepID || cause.Sequence >= ev.Sequence {
				t.Fatalf("%s caused by %s of another step or later", ev.Type, cause.Type)
			}
			causes[ev.Type] = cause.Type
		}
	}
	want := map[string]string{
		core.EvModelCompleted: core.EvModelStarted,
		core.EvToolStarted:    core.EvToolScheduled,
		core.EvToolCompleted:  core.EvToolStarted,
	}
	if !maps.Equal(causes, want) {
		t.Fatalf("causes=%v want %v", causes, want)
	}
}

func TestDelegationLimits(t *testing.T) {
	lead := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{delegateCall("1", "leaf", "skip a level")}},
		model.MockResponse{ToolCalls: []model.ToolCall{delegateCall("2", "mid", "go")}},
		model.MockResponse{Content: "done"},
	)
	mid := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{delegateCall("3", "leaf", "deeper")}},
		model.MockResponse{Content: "mid done"},
	)
	leaf := model.NewMockProvider()
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "lead", Model: "m", Provider: lead, Children: []string{"mid"}},
		{Name: "mid", Model: "m", Provider: mid, Children: []string{"leaf"}},
		{Name: "leaf", Model: "m", Provider: leaf},
	})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Limits: core.Limits{MaxDepth: 1}, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "lead", Prompt: "go"}}},
	}}})
	if err != nil || res.Outputs["s"] != "done" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if len(leaf.Calls()) != 0 {
		t.Fatal("a delegation outside the allow list or the depth limit ran")
	}
	if !strings.Contains(lastPrompt(lead.Calls()[1]), `may not delegate to \"leaf\"`) {
		t.Fatalf("refusal=%q", lastPrompt(lead.Calls()[1]))
	}
	if len(mid.Calls()[0].Tools) != 0 || !strings.Contains(lastPrompt(mid.Calls()[1]), "not available") {
		t.Fatal("an agent at the depth limit could still delegate")
	}
	tasks, _ := s.ListRunTasks(context.Background(), core.DefaultTenant, res.RunID)
	if len(tasks) != 2 || slices.ContainsFunc(tasks, func(t store.Task) bool { return t.Depth > 1 }) {
		t.Fatalf("tasks=%+v", tasks)
	}
}

func TestOutputSchemaFeedsBackInvalidAnswers(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["score"],"properties":{"score":{"type":"integer"}}}`)
	prov := model.NewMockProvider(
		model.MockResponse{Content: "not json"},
		model.MockResponse{Content: `{"score":"high"}`},
		model.MockResponse{Content: `{"score":3}`},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov}})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", OutputSchema: schema, Agents: []runtime.AgentStep{{Name: "a", Prompt: "score it"}}},
	}}})
	if err != nil || res.Outputs["s"] != `{"score":3}` {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	calls := prov.Calls()
	if string(calls[0].ResponseSchema) != string(schema) || !strings.Contains(lastPrompt(calls[1]), "does not match the required output schema") {
		t.Fatalf("schema not sent or rejection not fed back: %+v", calls)
	}
	if got := systemEvents(t, s, res.RunID, "output_rejected"); len(got) != 2 {
		t.Fatalf("output_rejected events=%v", got)
	}
}

func TestInputSchemaAndTemplatesCheckedAtStart(t *testing.T) {
	orch, s, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: model.NewMockProvider(model.MockResponse{Content: "hi"})}})
	orch.Workflows["greet"] = runtime.Workflow{
		Name:        "greet",
		InputSchema: json.RawMessage(`{"type":"object","required":["who"]}`),
		Stages:      []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "a", Prompt: "greet {{input.who}}"}}}},
	}
	ctx := context.Background()
	if _, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Template: "greet"}); err == nil || !strings.Contains(err.Error(), "input_schema") {
		t.Fatalf("input missing a required key: err=%v", err)
	}
	if _, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Template: "missing"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown template: err=%v", err)
	}
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Template: "greet", Input: map[string]string{"who": "Ada"}})
	if err != nil || res.Outputs["s"] != "hi" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	run, err := s.GetRun(ctx, core.DefaultTenant, res.RunID)
	if err != nil || !strings.Contains(string(run.Workflow), `"name":"greet"`) {
		t.Fatalf("stored workflow=%s err=%v", run.Workflow, err)
	}
}

func TestInvalidWorkflowsAreRejected(t *testing.T) {
	orch, s, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: model.NewMockProvider()}})
	step := []runtime.AgentStep{{Name: "a", Prompt: "x"}}
	for name, stages := range map[string][]runtime.Stage{
		"no stages":            nil,
		"duplicate stage":      {{Name: "s", Agents: step}, {Name: "s", Agents: step}},
		"unknown agent":        {{Name: "s", Agents: []runtime.AgentStep{{Name: "ghost"}}}},
		"unknown kind":         {{Name: "s", Kind: "random", Agents: step}},
		"unknown dependency":   {{Name: "s", DependsOn: []string{"ghost"}, Agents: step}},
		"cycle":                {{Name: "a", DependsOn: []string{"b"}, Agents: step}, {Name: "b", DependsOn: []string{"a"}, Agents: step}},
		"hand back forward":    {{Name: "a", HandBack: &runtime.HandBack{When: "x", To: "b"}, Agents: step}, {Name: "b", Agents: step}},
		"bad stop pattern":     {{Name: "s", StopWhen: "(", Agents: step}},
		"bad output schema":    {{Name: "s", OutputSchema: json.RawMessage(`{"type":5}`), Agents: step}},
		"schema loads a file":  {{Name: "s", OutputSchema: json.RawMessage(`{"$ref":"file:///etc/passwd"}`), Agents: step}},
		"negative retry count": {{Name: "s", Retry: runtime.RetryPolicy{MaxAttempts: -1}, Agents: step}},
		"quorum without count": {{Name: "s", Kind: runtime.StageParallel, FailurePolicy: runtime.Quorum, Agents: step}},
		"quorum above agents":  {{Name: "s", Kind: runtime.StageParallel, FailurePolicy: runtime.Quorum, Quorum: 2, Agents: step}},
		"count without quorum": {{Name: "s", Kind: runtime.StageParallel, Quorum: 1, Agents: step}},
		"sequential quorum":    {{Name: "s", FailurePolicy: runtime.Quorum, Quorum: 1, Agents: step}},
	} {
		if _, err := orch.StartRun(context.Background(), runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: stages}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if runs, _ := s.ListRuns(context.Background(), core.DefaultTenant, sid); len(runs) != 0 {
		t.Fatalf("invalid workflows created %d runs", len(runs))
	}
}

const sampleConfig = `{
  "agents": [
    {"name": "lead", "instructions": "Plan, then delegate.", "children": ["researcher"], "timeout": 60000000000},
    {"name": "researcher", "role": "finds facts", "tools": ["echo"]}
  ],
  "workflows": [
    {"name": "research", "input_schema": {"type": "object", "required": ["topic"]}, "stages": [
      {"name": "plan", "agents": [{"name": "lead", "prompt": "Research {{input.topic}}"}], "retry": {"max_attempts": 2}},
      {"name": "check", "agents": [{"name": "researcher", "prompt": "Check {{stage.plan}}"}], "hand_back": {"when": "^WRONG", "to": "plan"}}
    ]}
  ],
  "tool_policy": {"denylist": ["shell"], "approve_side_effects": ["write_external", "dangerous"]}
}`

func TestParseConfig(t *testing.T) {
	cfg, err := runtime.ParseConfig([]byte(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	if loop, err := runtime.ParseConfig([]byte(`{"agents":[{"name":"a"}],"loop_response":"approve","approve_above":{"total_tokens":100}}`)); err != nil ||
		loop.LoopResponse != runtime.LoopApprove || loop.ApproveAbove.TotalTokens != 100 {
		t.Fatalf("loop config=%+v err=%v", loop, err)
	}
	if c, err := runtime.ParseConfig([]byte(`{"agents":[{"name":"a"}],"compaction":{"agent":"a","task_max_tokens":100}}`)); err != nil || c.Compaction.TaskMaxTokens != 100 {
		t.Fatalf("task compaction config=%+v err=%v", c.Compaction, err)
	}
	if len(cfg.Agents) != 2 || cfg.Agents[0].Timeout != time.Minute || cfg.Workflows[0].Stages[1].HandBack.To != "plan" ||
		cfg.ToolPolicy == nil || len(cfg.ToolPolicy.ApproveSideEffects) != 2 || cfg.ToolPolicy.Denylist[0] != "shell" {
		t.Fatalf("config=%+v", cfg)
	}
	for name, bad := range map[string]string{
		"unknown field":    `{"agents":[{"name":"a","colour":"red"}]}`,
		"unknown child":    `{"agents":[{"name":"a","children":["b"]}]}`,
		"duplicate agent":  `{"agents":[{"name":"a"},{"name":"a"}]}`,
		"unknown agent":    `{"agents":[],"workflows":[{"name":"w","stages":[{"name":"s","agents":[{"name":"a"}]}]}]}`,
		"unnamed workflow": `{"agents":[{"name":"a"}],"workflows":[{"stages":[{"name":"s","agents":[{"name":"a"}]}]}]}`,
		"compaction agent": `{"agents":[{"name":"a"}],"compaction":{"agent":"b","max_messages":5}}`,
		"compaction never": `{"agents":[{"name":"a"}],"compaction":{"agent":"a"}}`,
		"side effect":      `{"agents":[{"name":"a"}],"tool_policy":{"approve_side_effects":["sends_email"]}}`,
		"artifact cap":     `{"agents":[{"name":"a"}],"max_artifact_bytes":-1}`,
		"negative limits":  `{"agents":[{"name":"a"}],"limits":{"default":{"max_tool_calls":-1}}}`,
		"negative pool":    `{"agents":[{"name":"a"}],"concurrency":{"per_agent":-1}}`,
		"negative timeout": `{"agents":[{"name":"a"}],"approval_timeout":-1}`,
		"tool timeout":     `{"agents":[{"name":"a"}],"tool_policy":{"timeouts":{"x":0}}}`,
		"loop response":    `{"agents":[{"name":"a"}],"loop_response":"panic"}`,
		"approve above":    `{"agents":[{"name":"a"}],"approve_above":{"cost_units":-1}}`,
	} {
		if _, err := runtime.ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func FuzzParseConfig(f *testing.F) {
	f.Add([]byte(sampleConfig))
	f.Add([]byte(`{"agents":[{"name":"a"}],"compaction":{"agent":"a","max_tokens":8000,"keep_recent":6}}`))
	f.Add([]byte(`{"agents":[{"name":"a"}],"tool_policy":{"budgets":{"echo":{"max_calls":2,"max_time":1000,"max_cost_units":-1}}}}`))
	f.Add([]byte(`{"agents":[{"name":"a"}],"max_artifact_bytes":1048576}`))
	f.Add([]byte(`{"agents":[{"name":"a"}],"limits":{"default":{"max_depth":2},"max":{"max_runtime":-5}},"concurrency":{"max_active_runs":1},"tool_retry":{"backoff":1}}`))
	f.Add([]byte(`{"agents":[{"name":"a"}],"loop_response":"pause","approve_above":{"total_tokens":100,"cost_units":5}}`))
	f.Add([]byte(`{"agents":[{"name":"a"}],"workflows":[{"name":"w","stages":[{"name":"s","depends_on":["s"],"agents":[{"name":"a"}]}]}]}`))
	f.Add([]byte(`{"agents":[{"name":"a"}],"workflows":[{"name":"w","stages":[{"name":"s","output_schema":{"$ref":"#/x"},"agents":[{"name":"a"}]}]}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = runtime.ParseConfig(data)
	})
}

func TestResumeFailedRun(t *testing.T) {
	planner := model.NewMockProvider(model.MockResponse{Content: "plan"})
	writer := model.NewMockProvider(model.MockResponse{Err: errors.New("provider down")})
	orch, s, sid := setup(t, []runtime.Agent{
		{Name: "planner", Model: "m", Provider: planner},
		{Name: "writer", Model: "m", Provider: writer},
	})
	in := runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "plan", Agents: []runtime.AgentStep{{Name: "planner", Prompt: "plan"}}},
		{Name: "write", Agents: []runtime.AgentStep{{Name: "writer", Prompt: "write {{stage.plan}}"}}},
	}}}
	res, err := runWorkflow(t, orch, in)
	if err == nil || res.Status != core.RunFailed {
		t.Fatalf("first attempt: status=%s err=%v", res.Status, err)
	}

	writer.Push(model.MockResponse{Content: "text"})
	ctx, cancel := waitCtx()
	defer cancel()
	resumer := core.PrincipalID("00000000-0000-0000-0000-0000000000b8")
	if err := orch.Resume(ctx, core.DefaultTenant, res.RunID, resumer); err != nil {
		t.Fatal(err)
	}
	res, err = orch.Wait(ctx, core.DefaultTenant, res.RunID)
	if err != nil || res.Outputs["write"] != "text" || res.FailureReason != "" {
		t.Fatalf("resumed: result=%+v err=%v", res, err)
	}
	if len(planner.Calls()) != 1 {
		t.Fatal("the completed plan stage ran again")
	}
	run, err := s.GetRun(ctx, core.DefaultTenant, res.RunID)
	if err != nil || run.Status != core.RunSucceeded || run.EndedAt.IsZero() {
		t.Fatalf("stored run=%+v err=%v", run, err)
	}
	if replayed, err := runtime.Replay(ctx, s, core.DefaultTenant, res.RunID); err != nil || replayed.Status != core.RunSucceeded {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, res.RunID, 0, 0)
	byResumer := 0
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), "resumed") {
			if ev.ActorID != resumer {
				t.Fatalf("%s %s: actor %s, want the resumer", ev.Type, ev.Payload, ev.ActorID)
			}
			byResumer++
		}
	}
	if byResumer != 2 {
		t.Fatalf("%d resumption events, want run_state_changed and run_resumed", byResumer)
	}
	if err := orch.Resume(ctx, core.DefaultTenant, res.RunID, core.DefaultPrincipal); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("resume of a succeeded run: err=%v want ErrConflict", err)
	}
}

func TestCanceledRunIsNotResumed(t *testing.T) {
	orch, _, sid := setup(t, []runtime.Agent{{Name: "slow", Model: "m", Provider: &slowProvider{delay: 5 * time.Second}}})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "slow", Prompt: "x"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := orch.Cancel(ctx, core.DefaultTenant, runID, "", "test"); err != nil {
		t.Fatal(err)
	}
	if res, _ := orch.Wait(ctx, core.DefaultTenant, runID); res.Status != core.RunCanceled {
		t.Fatalf("status=%s", res.Status)
	}
	if err := orch.Resume(ctx, core.DefaultTenant, runID, core.DefaultPrincipal); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("resume of a canceled run: err=%v want ErrConflict", err)
	}
}

// Acceptance: §Observability and Operations #1 – traces cross model, tool and sub-agent boundaries.
func TestRunIsTraced(t *testing.T) {
	lead := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{delegateCall("1", "researcher", "find X")}},
		model.MockResponse{Content: "summary"},
	)
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "lead", Model: "big", Provider: lead, Children: []string{"researcher"}},
		{Name: "researcher", Model: "small", Provider: model.NewMockProvider(model.MockResponse{Content: "42"})},
	})
	rec := tracetest.NewSpanRecorder()
	orch.Tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("test")
	if _, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Name: "wf", Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "lead", Prompt: "go"}}},
	}}}); err != nil {
		t.Fatal(err)
	}
	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, sp := range rec.Ended() {
		spans[sp.Name()] = sp
	}
	parentOf := map[string]string{
		"stage s":               "run wf",
		"task lead":             "stage s",
		"chat big":              "task lead",
		"execute_tool delegate": "task lead",
		"task researcher":       "execute_tool delegate",
		"chat small":            "task researcher",
	}
	for child, parent := range parentOf {
		c, p := spans[child], spans[parent]
		if c == nil || p == nil {
			t.Fatalf("missing span %q or %q; got %v", child, parent, slices.Collect(maps.Keys(spans)))
		}
		if c.Parent().SpanID() != p.SpanContext().SpanID() {
			t.Errorf("span %q is not a child of %q", child, parent)
		}
	}
	if spans["run wf"].Parent().IsValid() {
		t.Error("run span has a parent")
	}
}

func TestInputImagesReachTheModelAndSurviveResume(t *testing.T) {
	prov := model.NewMockProvider(model.MockResponse{Err: errors.New("down")})
	orch, s, sid := setup(t, []runtime.Agent{{Name: "looker", Model: "m", Provider: prov}})
	img := model.Image{MimeType: "image/png", Data: []byte("png-bytes")}
	res, err := runWorkflow(t, orch, runtime.StartRunInput{
		SessionID: sid,
		Images:    map[string]model.Image{"photo": img},
		Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "s", Agents: []runtime.AgentStep{{Name: "looker", Prompt: "describe", Images: []string{"photo"}}}},
		}},
	})
	if err == nil {
		t.Fatal("first attempt should fail")
	}
	artifacts, err := s.ListRunArtifacts(context.Background(), core.DefaultTenant, res.RunID)
	if err != nil || len(artifacts) != 1 || artifacts[0].Name != "photo" || string(artifacts[0].Content) != "png-bytes" {
		t.Fatalf("artifacts=%+v err=%v", artifacts, err)
	}

	prov.Push(model.MockResponse{Content: "a photo"})
	ctx, cancel := waitCtx()
	defer cancel()
	if err := orch.Resume(ctx, core.DefaultTenant, res.RunID, core.DefaultPrincipal); err != nil {
		t.Fatal(err)
	}
	if res, err = orch.Wait(ctx, core.DefaultTenant, res.RunID); err != nil {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	for i, call := range prov.Calls() {
		got := call.Messages[len(call.Messages)-1].Images
		if len(got) != 1 || got[0].MimeType != "image/png" || string(got[0].Data) != "png-bytes" {
			t.Fatalf("call %d images=%+v", i, got)
		}
	}
}

func TestStartRunRejectsUnknownImage(t *testing.T) {
	orch, _, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: model.NewMockProvider()}})
	_, err := orch.StartRun(context.Background(), runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "a", Prompt: "x", Images: []string{"missing"}}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("err=%v", err)
	}
}

// Acceptance: §Model Abstraction #2 – unsupported capabilities fail before model invocation.
func TestUnsupportedCapabilityFailsBeforeModelCall(t *testing.T) {
	for name, tc := range map[string]struct {
		step   runtime.AgentStep
		tools  []string
		reason string
	}{
		"images": {step: runtime.AgentStep{Name: "a", Prompt: "x", Images: []string{"photo"}}, reason: "does not accept images"},
		"tools":  {step: runtime.AgentStep{Name: "a", Prompt: "x"}, tools: []string{"echo"}, reason: "does not call tools"},
	} {
		t.Run(name, func(t *testing.T) {
			prov := &peakProvider{}
			orch, _, sid := setup(t, []runtime.Agent{{Name: "a", Model: "text-only", Provider: prov, Tools: tc.tools}})
			res, err := runWorkflow(t, orch, runtime.StartRunInput{
				SessionID: sid,
				Images:    map[string]model.Image{"photo": {MimeType: "image/png", Data: []byte("x")}},
				Workflow: runtime.Workflow{Stages: []runtime.Stage{
					{Name: "s", Retry: runtime.RetryPolicy{MaxAttempts: 3}, Agents: []runtime.AgentStep{tc.step}},
				}},
			})
			if err == nil || res.Status != core.RunFailed || !strings.Contains(res.FailureReason, tc.reason) {
				t.Fatalf("status=%s reason=%q err=%v", res.Status, res.FailureReason, err)
			}
			if prov.peak != 0 || res.Usage.SubAgents != 1 {
				t.Fatalf("model called (peak %d) or retried (%d attempts)", prov.peak, res.Usage.SubAgents)
			}
		})
	}
}

func FuzzDelegateArguments(f *testing.F) {
	f.Add([]byte(`{"agent":"helper","task":"look it up"}`))
	f.Add([]byte(`{"agent":"lead","task":"recurse"}`))
	f.Add([]byte(`{"agent":1}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, input []byte) {
		if !json.Valid(input) {
			return
		}
		prov := model.NewMockProvider(
			model.MockResponse{ToolCalls: []model.ToolCall{{ID: "c", Name: "delegate", Input: input}}},
			model.MockResponse{Content: "child or final"},
			model.MockResponse{Content: "final"},
		)
		orch, s, sid := setup(t, []runtime.Agent{
			{Name: "lead", Model: "m", Provider: prov, Children: []string{"helper"}},
			{Name: "helper", Model: "m", Provider: prov},
		})
		ctx, cancel := waitCtx()
		defer cancel()
		runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "s", Agents: []runtime.AgentStep{{Name: "lead", Prompt: "go"}}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Status != core.RunSucceeded {
			t.Fatalf("delegate input %q: status %s err %v", input, res.Status, err)
		}
		var req struct {
			Agent string `json:"agent"`
			Task  string `json:"task"`
		}
		valid := json.Unmarshal(input, &req) == nil && req.Agent == "helper"
		tasks, err := s.ListRunTasks(ctx, core.DefaultTenant, runID)
		if err != nil {
			t.Fatal(err)
		}
		if helped := slices.ContainsFunc(tasks, func(tk store.Task) bool { return tk.AgentName == "helper" }); helped != valid {
			t.Fatalf("delegate input %q: helper ran=%v", input, helped)
		}
	})
}

func FuzzStructuredOutput(f *testing.F) {
	f.Add(`{"n":1}`)
	f.Add(`{"n":"1"}`)
	f.Add("```json\n{\"n\":2}\n```")
	f.Add(`{"n":1e400}`)
	f.Fuzz(func(t *testing.T, content string) {
		prov := model.NewMockProvider(model.MockResponse{Content: content}, model.MockResponse{Content: `{"n":7}`})
		orch, _, sid := setup(t, []runtime.Agent{{Name: "a", Model: "m", Provider: prov}})
		ctx, cancel := waitCtx()
		defer cancel()
		runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name: "s", Agents: []runtime.AgentStep{{Name: "a", Prompt: "count"}},
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		}}}})
		if err != nil {
			t.Fatal(err)
		}
		res, err := orch.Wait(ctx, core.DefaultTenant, runID)
		if err != nil {
			t.Fatalf("content %q: %v", content, err)
		}
		var out struct {
			N *json.Number `json:"n"`
		}
		if json.Unmarshal([]byte(res.Outputs["s"]), &out) != nil || out.N == nil {
			t.Fatalf("content %q gave output %q", content, res.Outputs["s"])
		}
	})
}
