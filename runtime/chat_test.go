package runtime_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
)

// Acceptance: §Session and Run Lifecycle #8 – an agent asks a question and the next human message continues the run.
func TestAskHumanContinuesWithTheNextMessage(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "q", Name: "ask_human", Input: json.RawMessage(`{"question":"Which city?"}`)}}},
		model.MockResponse{Content: "Booked Hue"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "agent", Model: "m", Provider: prov, AskHuman: true}})
	ctx, cancel := waitCtx()
	defer cancel()
	if posted, err := orch.PostMessage(ctx, core.DefaultTenant, sid, core.DefaultPrincipal, "user", "hello", nil); err != nil || posted.Answered != "" {
		t.Fatalf("a message with no question open: %+v %v", posted, err)
	}
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("agent")})
	if err != nil {
		t.Fatal(err)
	}
	awaitApprovalRequest(t, s, runID)
	posted, err := orch.PostMessage(ctx, core.DefaultTenant, sid, core.DefaultPrincipal, "user", "Hue, please", nil)
	if err != nil || posted.Answered != runID || posted.Event.ActorID != core.DefaultPrincipal {
		t.Fatalf("posted=%+v err=%v", posted, err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Outputs["s"] != "Booked Hue" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	msgs := prov.Calls()[1].Messages
	if last := msgs[len(msgs)-1]; last.Role != model.RoleTool || !strings.Contains(last.Content, "Hue, please") {
		t.Fatalf("the agent got %+v", last)
	}
	if tools := prov.Calls()[0].Tools; len(tools) != 1 || tools[0].Name != "ask_human" {
		t.Fatalf("tools offered: %+v", tools)
	}
}

func TestUnansweredQuestionGoesBackToTheAgent(t *testing.T) {
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "q", Name: "ask_human", Input: json.RawMessage(`{"question":"Anyone there?"}`)}}},
		model.MockResponse{Content: "Going on alone"},
	)
	orch, _, sid := setup(t, []runtime.Agent{{Name: "agent", Model: "m", Provider: prov, AskHuman: true}})
	orch.ApprovalTimeout = 50 * time.Millisecond
	if res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("agent")}); err != nil || res.Outputs["s"] != "Going on alone" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	msgs := prov.Calls()[1].Messages
	if last := msgs[len(msgs)-1]; !strings.Contains(last.Content, `"answered":false`) || !strings.Contains(last.Content, "expired") {
		t.Fatalf("the agent got %+v", last)
	}
}

func askingAgent(t *testing.T, answers ...string) (*runtime.Orchestrator, store.Store, core.SessionID, *model.MockProvider) {
	t.Helper()
	prov := model.NewMockProvider()
	orch, s, sid := setup(t, []runtime.Agent{{Name: "agent", Model: "m", Provider: prov, AskHuman: true}})
	for range answers {
		prov.Push(model.MockResponse{ToolCalls: []model.ToolCall{{ID: "q", Name: "ask_human", Input: json.RawMessage(`{"question":"Which city?"}`)}}})
	}
	for _, a := range answers {
		prov.Push(model.MockResponse{Content: a})
	}
	return orch, s, sid, prov
}

func openQuestions(t *testing.T, s store.Store, n int) []store.Approval {
	t.Helper()
	ctx, cancel := waitCtx()
	defer cancel()
	for ctx.Err() == nil {
		open, _ := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{Status: "requested"})
		if len(open) >= n {
			return open
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d questions were asked", n)
	return nil
}

func TestQuestionsDoNotHoldAToolSlot(t *testing.T) {
	orch, s, sid, _ := askingAgent(t, "a", "b")
	orch.Concurrency.PerTool = 1
	ctx, cancel := waitCtx()
	defer cancel()
	for range 2 {
		if _, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("agent")}); err != nil {
			t.Fatal(err)
		}
	}
	openQuestions(t, s, 2)
}

func TestConcurrentMessagesAnswerEveryQuestion(t *testing.T) {
	orch, s, sid, _ := askingAgent(t, "a", "b")
	ctx, cancel := waitCtx()
	defer cancel()
	var runs []core.RunID
	for range 2 {
		id, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("agent")})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, id)
	}
	openQuestions(t, s, 2)
	var wg sync.WaitGroup
	answered := make(chan core.RunID, 2)
	for _, text := range []string{"Hue", "Hanoi"} {
		wg.Go(func() {
			posted, err := orch.PostMessage(ctx, core.DefaultTenant, sid, core.DefaultPrincipal, "user", text, nil)
			if err != nil {
				t.Error(err)
			}
			answered <- posted.Answered
		})
	}
	wg.Wait()
	close(answered)
	for id := range answered {
		if id == "" {
			t.Fatal("a message answered no question while one was open")
		}
	}
	for _, id := range runs {
		if _, err := orch.Wait(ctx, core.DefaultTenant, id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReExecuteARunThatAsked(t *testing.T) {
	orch, s, sid, _ := askingAgent(t, "Booked Hue")
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("agent")})
	if err != nil {
		t.Fatal(err)
	}
	openQuestions(t, s, 1)
	if _, err := orch.PostMessage(ctx, core.DefaultTenant, sid, core.DefaultPrincipal, "user", "Hue", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}
	again, err := orch.ReExecute(ctx, core.DefaultTenant, runID, core.SystemPrincipal)
	if err != nil || again.Outputs["s"] != "Booked Hue" {
		t.Fatalf("re-executed: %+v %v", again, err)
	}
	if open, _ := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{Status: "requested"}); len(open) != 0 {
		t.Fatalf("re-execution asked again: %+v", open)
	}
}

func TestQuestionsAnsweredOverREST(t *testing.T) {
	for decision, want := range map[string]string{"approved": `"answer":"Da Nang"`, "rejected": `"status":"rejected"`} {
		t.Run(decision, func(t *testing.T) {
			orch, s, sid, prov := askingAgent(t, "ok")
			ctx, cancel := waitCtx()
			defer cancel()
			runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("agent")})
			if err != nil {
				t.Fatal(err)
			}
			q := openQuestions(t, s, 1)[0]
			if err := s.ResolveApproval(ctx, core.DefaultTenant, q.ID, decision, core.DefaultPrincipal, json.RawMessage(`{"answer":"Da Nang"}`)); err != nil {
				t.Fatal(err)
			}
			if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
				t.Fatal(err)
			}
			msgs := prov.Calls()[1].Messages
			if last := msgs[len(msgs)-1]; !strings.Contains(last.Content, want) {
				t.Fatalf("the agent got %q, want %s", last.Content, want)
			}
		})
	}
}

func FuzzAskHumanArguments(f *testing.F) {
	f.Add([]byte(`{"question":"Which city?"}`), []byte(`{"answer":"Hue"}`))
	f.Add([]byte(`{"question":""}`), []byte(`{"message":5}`))
	f.Add([]byte(`[]`), []byte(`null`))
	f.Fuzz(func(t *testing.T, input, payload []byte) {
		if !json.Valid(input) || !json.Valid(payload) {
			return
		}
		prov := model.NewMockProvider(
			model.MockResponse{ToolCalls: []model.ToolCall{{ID: "q", Name: "ask_human", Input: input}}},
			model.MockResponse{Content: "done"},
		)
		orch, s, sid := setup(t, []runtime.Agent{{Name: "agent", Model: "m", Provider: prov, AskHuman: true}})
		orch.ApprovalTimeout = 200 * time.Millisecond
		ctx, cancel := waitCtx()
		defer cancel()
		runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("agent")})
		if err != nil {
			t.Fatal(err)
		}
		for ctx.Err() == nil {
			if open, _ := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{RunID: runID, Status: "requested"}); len(open) == 1 {
				_ = s.ResolveApproval(ctx, core.DefaultTenant, open[0].ID, "approved", core.DefaultPrincipal, payload)
			}
			if run, _ := s.GetRun(ctx, core.DefaultTenant, runID); run.Status.Terminal() {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if res, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Outputs["s"] != "done" {
			t.Fatalf("input %q payload %q: %+v %v", input, payload, res, err)
		}
	})
}
