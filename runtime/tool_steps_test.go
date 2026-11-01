package runtime_test

import (
	"context"
	"encoding/json"
	"strconv"
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

// callsTool returns a mock that calls name n times, with different inputs, then answers "done".
func callsTool(name string, n int) *model.MockProvider {
	prov := model.NewMockProvider()
	for i := range n {
		prov.Push(model.MockResponse{ToolCalls: []model.ToolCall{{ID: "c", Name: name, Input: json.RawMessage(`{"text":"` + strconv.Itoa(i) + `"}`)}}})
	}
	prov.Push(model.MockResponse{Content: "done"})
	return prov
}

func toolResults(prov *model.MockProvider) []string {
	var out []string
	for _, req := range prov.Calls() {
		if last := req.Messages[len(req.Messages)-1]; last.Role == model.RoleTool {
			out = append(out, last.Content)
		}
	}
	return out
}

// Acceptance: §Persistence and Replay #5 – a failed step is retried only when that is safe.
func TestTransientToolFailuresAreRetriedWhenSafe(t *testing.T) {
	for _, tc := range []struct {
		idem  core.IdempotencyHint
		calls int
	}{{core.IdemIdempotent, 3}, {core.IdemConditionalIdempotent, 3}, {core.IdemNonIdempotent, 1}, {core.IdemUnknown, 1}} {
		t.Run(string(tc.idem), func(t *testing.T) {
			prov := callsTool("flaky", 1)
			orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"flaky"}}})
			orch.ToolRetry.Backoff = time.Millisecond
			var mu sync.Mutex
			var keys []string
			if err := orch.Tools.Register(core.ToolDefinition{Name: "flaky", Idempotency: tc.idem, SideEffect: core.SideNone},
				tool.ExecutorFunc(func(_ context.Context, c tool.Call) (tool.Result, error) {
					mu.Lock()
					defer mu.Unlock()
					keys = append(keys, c.IdempotencyKey)
					if len(keys) < 3 {
						return tool.Result{}, tool.ErrTransient
					}
					return tool.Result{Output: json.RawMessage(`{"ok":true}`)}, nil
				})); err != nil {
				t.Fatal(err)
			}
			res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
			if err != nil || res.Outputs["s"] != "done" {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if len(keys) != tc.calls || keys[0] == "" || keys[len(keys)-1] != keys[0] {
				t.Fatalf("calls with keys %q, want %d with one key", keys, tc.calls)
			}
			if got := toolResults(prov); tc.calls == 3 && got[0] != `{"ok":true}` || tc.calls == 1 && !strings.Contains(got[0], "transient") {
				t.Fatalf("the agent got %q", got)
			}
			if retried := systemEvents(t, s, res.RunID, "tool_call_retried"); len(retried) != tc.calls-1 || res.Usage.ToolCalls != int64(tc.calls) {
				t.Fatalf("%d retry events, %d tool calls counted", len(retried), res.Usage.ToolCalls)
			}
		})
	}
}

// Acceptance: §Budget and Safety #6 – a tool's calls, time and cost are budgeted per run.
func TestToolBudgets(t *testing.T) {
	cases := map[string]struct {
		budget policy.ToolBudget
		cost   int64
		want   []string
	}{
		"calls": {budget: policy.ToolBudget{MaxCalls: 1}, want: []string{`{"ok":true}`, "may be called 1 times", "may be called 1 times"}},
		"time":  {budget: policy.ToolBudget{MaxTime: 100 * time.Millisecond}, want: []string{`{"ok":true}`, "rest of its time budget", "used its 100ms"}},
		"cost":  {budget: policy.ToolBudget{MaxCostUnits: 5}, cost: 5, want: []string{`{"ok":true}`, "used its 5 cost units", "used its 5 cost units"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			prov := callsTool("work", 3)
			orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"work"}}})
			orch.Policy.Budgets = map[string]policy.ToolBudget{"work": tc.budget}
			first := true
			if err := orch.Tools.Register(core.ToolDefinition{Name: "work", SideEffect: core.SideNone},
				tool.ExecutorFunc(func(ctx context.Context, _ tool.Call) (tool.Result, error) {
					if !first && tc.budget.MaxTime > 0 {
						<-ctx.Done()
						return tool.Result{}, ctx.Err()
					}
					first = false
					return tool.Result{Output: json.RawMessage(`{"ok":true}`), CostUnits: tc.cost}, nil
				})); err != nil {
				t.Fatal(err)
			}
			res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
			if err != nil || res.Outputs["s"] != "done" || res.Usage.CostUnits != tc.cost {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			got := toolResults(prov)
			for i, want := range tc.want {
				if !strings.Contains(got[i], want) {
					t.Fatalf("call %d: the agent got %q, want %q", i+1, got[i], want)
				}
			}
		})
	}
}

func TestEveryToolBudgetApplies(t *testing.T) {
	prov := callsTool("echo", 2)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
	orch.Policy.Budgets = map[string]policy.ToolBudget{"echo": {MaxCalls: 5}}
	wf := oneStage("ag")
	wf.ToolPolicy = &policy.ToolPolicy{Budgets: map[string]policy.ToolBudget{"echo": {MaxCalls: 1}}}
	if _, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: wf}); err != nil {
		t.Fatal(err)
	}
	if got := toolResults(prov); !strings.Contains(got[1], "may be called 1 times") {
		t.Fatalf("the workflow's budget did not apply: %q", got)
	}
}

func TestRejectedCallLeavesTheToolBudget(t *testing.T) {
	prov := callsTool("echo", 2)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
	orch.Policy.Budgets = map[string]policy.ToolBudget{"echo": {MaxCalls: 1}}
	orch.Policy.ApprovalRequired = []string{"echo"}
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, awaitApprovalRequest(t, s, runID), "rejected", core.DefaultPrincipal, nil); err != nil {
		t.Fatal(err)
	}
	for second := core.ApprovalID(""); second == "" && ctx.Err() == nil; time.Sleep(10 * time.Millisecond) {
		if open, _ := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{RunID: runID, Status: "requested"}); len(open) == 1 {
			second = open[0].ID
			if err := s.ResolveApproval(ctx, core.DefaultTenant, second, "approved", core.DefaultPrincipal, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}
	if got := toolResults(prov); !strings.Contains(got[0], "not approved") || strings.Contains(got[1], "error") {
		t.Fatalf("the agent got %q", got)
	}
}

func TestTimedOutCallOfAnIdempotentToolIsRetried(t *testing.T) {
	prov := callsTool("slow", 1)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"slow"}}})
	orch.ToolRetry.Backoff = time.Millisecond
	calls := 0
	if err := orch.Tools.Register(core.ToolDefinition{Name: "slow", Idempotency: core.IdemIdempotent, SideEffect: core.SideNone, Timeout: 20 * time.Millisecond},
		tool.ExecutorFunc(func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			if calls++; calls == 1 {
				<-ctx.Done()
				return tool.Result{}, ctx.Err()
			}
			return tool.Result{Output: json.RawMessage(`{"ok":true}`)}, nil
		})); err != nil {
		t.Fatal(err)
	}
	if res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")}); err != nil || calls != 2 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, calls)
	}
}

func TestRetryPastTheRunsToolCallsGoesBackToTheModel(t *testing.T) {
	prov := callsTool("flaky", 1)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"flaky"}}})
	if err := orch.Tools.Register(core.ToolDefinition{Name: "flaky", Idempotency: core.IdemIdempotent, SideEffect: core.SideNone},
		tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) { return tool.Result{}, tool.ErrTransient })); err != nil {
		t.Fatal(err)
	}
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag"), Limits: core.Limits{MaxToolCalls: 1}})
	if err != nil || res.Outputs["s"] != "done" || !strings.Contains(toolResults(prov)[0], "transient") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// Acceptance: §Persistence and Replay #5 – a tool's cost that ends the run ends its re-execution alike.
func TestReExecuteToolCostOverrun(t *testing.T) {
	prov := callsTool("pricey", 1)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"pricey"}}})
	if err := orch.Tools.Register(core.ToolDefinition{Name: "pricey", SideEffect: core.SideNone},
		tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
			return tool.Result{Output: json.RawMessage(`{"ok":true}`), CostUnits: 5}, nil
		})); err != nil {
		t.Fatal(err)
	}
	res, _ := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag"), Limits: core.Limits{MaxCostUnits: 4}})
	if res.Status != core.RunFailed || !strings.Contains(res.FailureReason, "cost units") {
		t.Fatalf("res=%+v", res)
	}
	ctx, cancel := waitCtx()
	defer cancel()
	again, _ := orch.ReExecute(ctx, core.DefaultTenant, res.RunID, core.SystemPrincipal)
	if again.Status != res.Status || again.FailureReason != res.FailureReason {
		t.Fatalf("original %s %q, copy %s %q", res.Status, res.FailureReason, again.Status, again.FailureReason)
	}
}
