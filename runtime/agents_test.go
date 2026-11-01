package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
)

// Acceptance: §Agent Definition #1 – a tenant defines agents over the API and its runs use them.
func TestTenantDefinedAgents(t *testing.T) {
	prov := model.NewMockProvider()
	orch, s, sid := setup(t, []runtime.Agent{{Name: "lead", Model: "m", Provider: model.NewMockProvider()}})
	orch.Provider, orch.Model = prov, "server-model"
	ctx, cancel := waitCtx()
	defer cancel()
	for name, a := range map[string]runtime.Agent{
		"configured name": {Name: "lead"},
		"unknown tool":    {Name: "x", Tools: []string{"nope"}},
		"unknown child":   {Name: "x", Children: []string{"ghost"}},
		"bad name":        {Name: "bad name!"},
		"negative":        {Name: "x", MaxToolLoops: -1},
	} {
		if _, err := orch.PutAgent(ctx, core.DefaultTenant, a); !errors.Is(err, runtime.ErrInvalidAgent) && !errors.Is(err, core.ErrConflict) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if v, err := orch.PutAgent(ctx, core.DefaultTenant, runtime.Agent{Name: "writer", Instructions: "Write.", Tools: []string{"echo"}}); err != nil || v != 1 {
		t.Fatalf("v=%d err=%v", v, err)
	}
	if v, err := orch.PutAgent(ctx, core.DefaultTenant, runtime.Agent{Name: "boss", Children: []string{"writer"}, Model: "big"}); err != nil || v != 1 {
		t.Fatalf("v=%d err=%v", v, err)
	}
	prov.Push(model.MockResponse{ToolCalls: []model.ToolCall{{ID: "d", Name: "delegate", Input: json.RawMessage(`{"agent":"writer","task":"draft"}`)}}})
	prov.Push(model.MockResponse{Content: "drafted"})
	prov.Push(model.MockResponse{Content: "done"})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("boss")})
	if err != nil || res.Outputs["s"] != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	calls := prov.Calls()
	if calls[0].Model != "big" || calls[1].Model != "server-model" || calls[1].Messages[0].Content != "Write." {
		t.Fatalf("models %q %q, writer saw %+v", calls[0].Model, calls[1].Model, calls[1].Messages[0])
	}

	other := core.TenantID(core.NewID())
	osid, _ := s.CreateSession(ctx, store.Session{TenantID: other, CreatedBy: core.DefaultPrincipal})
	if _, err := orch.StartRun(ctx, runtime.StartRunInput{TenantID: other, SessionID: osid, Workflow: oneStage("writer")}); err == nil {
		t.Fatal("another tenant ran this tenant's agent")
	}
	if err := orch.DeleteAgent(ctx, core.DefaultTenant, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("writer")}); err == nil {
		t.Fatal("a deleted agent still runs")
	}
	if err := orch.DeleteAgent(ctx, core.DefaultTenant, "lead"); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("deleting a configured agent: %v", err)
	}
	agents, err := orch.ListAgents(context.Background(), core.DefaultTenant)
	if err != nil || len(agents) != 2 || agents[0].Name != "boss" || agents[0].Source != "api" || agents[1].Source != "config" {
		t.Fatalf("agents=%+v err=%v", agents, err)
	}
}

func TestTenantAgentsResumeAndReExecute(t *testing.T) {
	prov := model.NewMockProvider(model.MockResponse{Err: errors.New("provider down")})
	orch, s, sid := setup(t, nil)
	orch.Provider, orch.Model = prov, "m"
	ctx, cancel := waitCtx()
	defer cancel()
	if _, err := orch.PutAgent(ctx, core.DefaultTenant, runtime.Agent{Name: "w", Instructions: "v1"}); err != nil {
		t.Fatal(err)
	}
	res, _ := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("w")})
	if res.Status != core.RunFailed {
		t.Fatalf("first attempt: %+v", res)
	}
	prov.Push(model.MockResponse{Content: "written"})
	if err := orch.Resume(ctx, core.DefaultTenant, res.RunID, core.DefaultPrincipal); err != nil {
		t.Fatal(err)
	}
	if res, err := orch.Wait(ctx, core.DefaultTenant, res.RunID); err != nil || res.Outputs["s"] != "written" {
		t.Fatalf("resumed: %+v %v", res, err)
	}
	if again, err := orch.ReExecute(ctx, core.DefaultTenant, res.RunID, core.SystemPrincipal); err != nil || again.Outputs["s"] != "written" {
		t.Fatalf("re-executed: %+v %v", again, err)
	}
	if _, err := orch.PutAgent(ctx, core.DefaultTenant, runtime.Agent{Name: "w", Instructions: "v2"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := orch.ReExecute(ctx, core.DefaultTenant, res.RunID, core.SystemPrincipal); again.Status == core.RunSucceeded {
		t.Fatal("re-execution after the agent changed did not diverge")
	}
	if _, err := s.PutAgent(ctx, store.AgentDefinition{TenantID: core.DefaultTenant, Name: "broken", Definition: json.RawMessage(`{"timeout":"30s"}`)}); err != nil {
		t.Fatal(err)
	}
	if agents, err := orch.ListAgents(ctx, core.DefaultTenant); err != nil || len(agents) != 1 || agents[0].Name != "w" {
		t.Fatalf("a definition that does not decode broke the tenant: %+v %v", agents, err)
	}
	orch.Provider = nil
	if _, err := orch.PutAgent(ctx, core.DefaultTenant, runtime.Agent{Name: "x"}); !errors.Is(err, runtime.ErrInvalidAgent) {
		t.Fatalf("a server without a provider took an agent: %v", err)
	}
}
