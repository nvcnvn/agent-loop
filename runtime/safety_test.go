package runtime_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
)

func echoTwice() []model.MockResponse {
	call := model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo", Input: json.RawMessage(`{"text":"again"}`)}}}
	return []model.MockResponse{call, call}
}

func oneStage(agent string) runtime.Workflow {
	return runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: agent, Prompt: "go"}}}}}
}

// Acceptance: §Safety – the same draft handed back again and again is a loop.
func TestNearIdenticalHandBackDraftsFailTheRun(t *testing.T) {
	writer, reviewer := model.NewMockProvider(), model.NewMockProvider()
	for range 5 {
		writer.Push(model.MockResponse{Content: "The answer is 42, as the tables show."})
		reviewer.Push(model.MockResponse{Content: "REJECT"})
	}
	orch, s, sid := setup(t, []runtime.Agent{{Name: "writer", Model: "m", Provider: writer}, {Name: "reviewer", Model: "m", Provider: reviewer}})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "draft", Agents: []runtime.AgentStep{{Name: "writer", Prompt: "write {{feedback}}"}}},
		{Name: "review", Agents: []runtime.AgentStep{{Name: "reviewer", Prompt: "{{stage.draft}}"}}, HandBack: &runtime.HandBack{When: "REJECT", To: "draft", MaxTimes: 5}},
	}}})
	if err == nil || !strings.Contains(res.FailureReason, "3 near-identical drafts") {
		t.Fatalf("status=%s reason=%q err=%v", res.Status, res.FailureReason, err)
	}
	if n := len(writer.Calls()); n != 3 {
		t.Fatalf("writer called %d times", n)
	}
	if loops := systemEvents(t, s, res.RunID, "loop_detected"); len(loops) != 1 || loops[0]["kind"] != "repeated_drafts" {
		t.Fatalf("loop events: %v", loops)
	}
}

// Acceptance: §Safety – delegating the same task again and again is a loop.
func TestRepeatedDelegationFailsTheRun(t *testing.T) {
	prov := model.NewMockProvider()
	for range 3 {
		prov.Push(model.MockResponse{ToolCalls: []model.ToolCall{{ID: "d", Name: "delegate", Input: json.RawMessage(`{"agent":"helper","task":"Find the price of item 7."}`)}}})
		prov.Push(model.MockResponse{Content: "I could not find it."})
	}
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "lead", Model: "m", Provider: prov, Children: []string{"helper"}, MaxToolLoops: 8},
		{Name: "helper", Model: "m", Provider: prov},
	})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("lead")})
	if err == nil || !strings.Contains(res.FailureReason, "delegated 3 near-identical tasks") {
		t.Fatalf("status=%s reason=%q err=%v", res.Status, res.FailureReason, err)
	}
}

// Acceptance: §Safety – retries piling up within a window are a retry storm.
func TestRetryStormFailsTheRun(t *testing.T) {
	prov := model.NewMockProvider()
	for range 10 {
		prov.Push(model.MockResponse{Err: errors.New("provider down")})
	}
	orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov}})
	orch.LoopDet.RetryStorm = 3
	wf := oneStage("ag")
	wf.Stages[0].Retry = runtime.RetryPolicy{MaxAttempts: 10, Backoff: time.Millisecond}
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: wf})
	if err == nil || !strings.Contains(res.FailureReason, "3 retries within") || len(prov.Calls()) != 3 {
		t.Fatalf("status=%s reason=%q err=%v calls=%d", res.Status, res.FailureReason, err, len(prov.Calls()))
	}
}

// Acceptance: §Safety – a loop can pause its run, which waits to be resumed or canceled.
func TestLoopPausesRun(t *testing.T) {
	prov := model.NewMockProvider(echoTwice()...)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
	orch.LoopDet = policy.NewLoopDetector(2)
	orch.LoopResponse = runtime.LoopPause
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
	if err == nil || res.Status != core.RunPaused {
		t.Fatalf("status=%s err=%v", res.Status, err)
	}
	ctx, cancel := waitCtx()
	defer cancel()
	if run, _ := s.GetRun(ctx, core.DefaultTenant, res.RunID); run.Status != core.RunPaused || !run.EndedAt.IsZero() {
		t.Fatalf("stored run: %+v", run)
	}
	if again, err := orch.Wait(ctx, core.DefaultTenant, res.RunID); again.Status != core.RunPaused || err == nil {
		t.Fatalf("wait on a paused run: %+v %v", again, err)
	}
	if n, err := orch.Recover(ctx); n != 0 || err != nil {
		t.Fatalf("recover resumed %d paused runs: %v", n, err)
	}

	prov.Push(model.MockResponse{Content: "done"})
	if err := orch.Resume(ctx, core.DefaultTenant, res.RunID, core.DefaultPrincipal); err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, res.RunID); err != nil || res.Outputs["s"] != "done" {
		t.Fatalf("resumed: %+v %v", res, err)
	}

	for _, r := range echoTwice() {
		prov.Push(r)
	}
	paused, _ := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
	if err := orch.Cancel(ctx, core.DefaultTenant, paused.RunID, core.DefaultPrincipal, "enough"); err != nil {
		t.Fatal(err)
	}
	if run, _ := s.GetRun(ctx, core.DefaultTenant, paused.RunID); run.Status != core.RunCanceled || run.EndedAt.IsZero() {
		t.Fatalf("canceled paused run: %+v", run)
	}
}

// Acceptance: §Safety – a loop can ask a human whether the run may carry on.
func TestLoopAsksForApproval(t *testing.T) {
	for _, decision := range []string{"approved", "rejected"} {
		t.Run(decision, func(t *testing.T) {
			prov := model.NewMockProvider(append(echoTwice(), model.MockResponse{Content: "done"})...)
			orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
			orch.LoopDet = policy.NewLoopDetector(2)
			orch.LoopResponse = runtime.LoopApprove
			ctx, cancel := waitCtx()
			defer cancel()
			wf := oneStage("ag")
			wf.Stages[0].Retry = runtime.RetryPolicy{MaxAttempts: 3}
			runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: wf})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ResolveApproval(ctx, core.DefaultTenant, awaitApprovalRequest(t, s, runID), decision, core.DefaultPrincipal, nil); err != nil {
				t.Fatal(err)
			}
			res, err := orch.Wait(ctx, core.DefaultTenant, runID)
			if decision == "approved" && (err != nil || res.Outputs["s"] != "done") || decision == "rejected" && res.Status != core.RunFailed {
				t.Fatalf("status=%s outputs=%v err=%v", res.Status, res.Outputs, err)
			}
			if asked, _ := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{RunID: runID}); len(asked) != 1 {
				t.Fatalf("%d approvals asked; a task that hit a loop must not be retried", len(asked))
			}
		})
	}
}

// Acceptance: §Human Approval – a run past its cost threshold asks once before calling models again.
func TestCostThresholdAsksOnce(t *testing.T) {
	for _, decision := range []string{"approved", "rejected"} {
		t.Run(decision, func(t *testing.T) {
			used := core.Usage{TotalTokens: 20, ModelCalls: 1}
			prov := model.NewMockProvider(
				model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}}, Usage: used},
				model.MockResponse{Content: "first", Usage: used},
				model.MockResponse{Content: "second", Usage: used},
			)
			orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}}})
			orch.ApproveAbove = core.Usage{TotalTokens: 10}
			ctx, cancel := waitCtx()
			defer cancel()
			runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
				{Name: "a", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "one"}}},
				{Name: "b", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "two"}}},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ResolveApproval(ctx, core.DefaultTenant, awaitApprovalRequest(t, s, runID), decision, core.DefaultPrincipal, nil); err != nil {
				t.Fatal(err)
			}
			res, err := orch.Wait(ctx, core.DefaultTenant, runID)
			if decision == "approved" && (err != nil || res.Outputs["b"] != "second") || decision == "rejected" && !strings.Contains(res.FailureReason, "cost approval threshold") {
				t.Fatalf("status=%s outputs=%v reason=%q err=%v", res.Status, res.Outputs, res.FailureReason, err)
			}
			requested, _ := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{RunID: runID})
			if len(requested) != 1 {
				t.Fatalf("%d approvals asked", len(requested))
			}
		})
	}
}

func TestCostThresholdAsksAgainWhenTheAskerGivesUp(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}}, Usage: core.Usage{TotalTokens: 20}},
		model.MockResponse{Content: "done"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"echo"}, Timeout: 200 * time.Millisecond}})
	orch.ApproveAbove = core.Usage{TotalTokens: 10}
	ctx, cancel := waitCtx()
	defer cancel()
	wf := oneStage("ag")
	wf.Stages[0].Retry = runtime.RetryPolicy{MaxAttempts: 2, Backoff: time.Millisecond}
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: wf})
	if err != nil {
		t.Fatal(err)
	}
	var second core.ApprovalID
	for second == "" && ctx.Err() == nil {
		open, _ := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{RunID: runID})
		if len(open) == 2 && open[1].Status == "requested" {
			second = open[1].ID
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, second, "approved", core.DefaultPrincipal, nil); err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Outputs["s"] != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestPauseStopsABestEffortStage(t *testing.T) {
	looping := model.NewMockProvider(echoTwice()...)
	fine := model.NewMockProvider(model.MockResponse{Content: "fine"})
	orch, _, sid := setup(t, []runtime.Agent{
		{Name: "loops", Model: "m", Provider: looping, Tools: []string{"echo"}},
		{Name: "fine", Model: "m", Provider: fine},
	})
	orch.LoopDet = policy.NewLoopDetector(2)
	orch.LoopResponse = runtime.LoopPause
	res, _ := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{
		Name: "s", Kind: runtime.StageParallel, FailurePolicy: runtime.BestEffort,
		Agents: []runtime.AgentStep{{Name: "loops", Prompt: "x"}, {Name: "fine", Prompt: "y"}},
	}}}})
	if res.Status != core.RunPaused {
		t.Fatalf("status=%s reason=%q", res.Status, res.FailureReason)
	}
}
