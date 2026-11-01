package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

// Acceptance: §Security #1 – a tenant's own tools and tool policy apply to its runs only.
func TestTenantScopedTools(t *testing.T) {
	prov := model.NewMockProvider()
	orch, s, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"crm", "echo"}}})
	other := core.TenantID(core.NewID())
	osid, _ := s.CreateSession(context.Background(), store.Session{TenantID: other, CreatedBy: core.DefaultPrincipal})
	own := tool.NewRegistry()
	if err := own.Register(core.ToolDefinition{Name: "crm", SideEffect: core.SideNone}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{Output: json.RawMessage(`{"customer":"Linh"}`)}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := own.Register(core.ToolDefinition{Name: "echo", SideEffect: core.SideNone}, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{Output: json.RawMessage(`{"own_echo":true}`)}, nil
	})); err != nil {
		t.Fatal(err)
	}
	orch.TenantTools = map[core.TenantID]*tool.Registry{core.DefaultTenant: own}
	orch.TenantPolicies = map[core.TenantID]policy.ToolPolicy{other: {Denylist: []string{"echo"}}}

	prov.Push(model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "crm", Input: json.RawMessage(`{}`)}, {ID: "2", Name: "echo", Input: json.RawMessage(`{"text":"hi"}`)}}})
	prov.Push(model.MockResponse{Content: "found Linh"})
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
	if err != nil || res.Outputs["s"] != "found Linh" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if tools := prov.Calls()[0].Tools; len(tools) != 2 {
		t.Fatalf("this tenant's agent was offered %+v", tools)
	}
	if out := toolOutputs(t, s, core.DefaultTenant, res.RunID); !strings.Contains(out["crm"], "Linh") || !strings.Contains(out["echo"], "own_echo") {
		t.Fatalf("stored tool results %v; want the tenant's crm and its own echo", out)
	}

	prov.Push(model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "crm", Input: json.RawMessage(`{}`)}}})
	prov.Push(model.MockResponse{Content: "no crm"})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{TenantID: other, SessionID: osid, Workflow: oneStage("ag")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Wait(ctx, other, runID); err != nil {
		t.Fatal(err)
	}
	calls := prov.Calls()
	if offered := calls[2].Tools; len(offered) != 0 {
		t.Fatalf("the other tenant's agent was offered %+v", offered)
	}
	if out := toolOutputs(t, s, other, runID); strings.Contains(out["crm"], "Linh") || !strings.Contains(out["crm"], "not available") {
		t.Fatalf("the other tenant's stored crm result %q", out["crm"])
	}
	if events, _ := s.ListRunEvents(ctx, other, runID, 0, 0); !slices.ContainsFunc(events, func(ev core.Event) bool {
		return ev.Type == core.EvSystem && strings.Contains(string(ev.Payload), "tool_denied") && strings.Contains(string(ev.Payload), "echo")
	}) {
		t.Fatal("the other tenant's policy did not deny echo")
	}

	if names := orch.ListTools(core.DefaultTenant); !hasTool(names, "crm") || hasTool(orch.ListTools(other), "crm") {
		t.Fatalf("tool lists: %v / %v", names, orch.ListTools(other))
	}
	orch.Provider = prov
	if _, err := orch.PutAgent(ctx, other, runtime.Agent{Name: "spy", Tools: []string{"crm"}}); !errors.Is(err, runtime.ErrInvalidAgent) {
		t.Fatalf("another tenant defined an agent with this tenant's tool: %v", err)
	}
	if again, err := orch.ReExecute(ctx, core.DefaultTenant, res.RunID, core.SystemPrincipal); err != nil || again.Outputs["s"] != "found Linh" {
		t.Fatalf("re-execute: %+v %v", again, err)
	}
}

func toolOutputs(t *testing.T, s store.Store, tenant core.TenantID, runID core.RunID) map[string]string {
	t.Helper()
	events, err := s.ListRunEvents(context.Background(), tenant, runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, ev := range events {
		if ev.Type == core.EvToolCompleted {
			var p struct{ Tool string }
			_ = json.Unmarshal(ev.Payload, &p)
			out[p.Tool] = string(ev.Payload)
		}
	}
	return out
}

func TestTenantToolPoliciesConfig(t *testing.T) {
	for _, bad := range []string{
		`{"agents":[{"name":"a"}],"tenant_tool_policies":{"acme":{}}}`,
		`{"agents":[{"name":"a"}],"tenant_tool_policies":{"00000000-0000-0000-0000-000000000002":{"budgets":{"echo":{"max_calls":-1}}}}}`,
	} {
		if _, err := runtime.ParseConfig([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	cfg, err := runtime.ParseConfig([]byte(`{"agents":[{"name":"a"}],"tenant_tool_policies":{"00000000-0000-0000-0000-000000000002":{"denylist":["echo"]}}}`))
	if err != nil || len(cfg.TenantToolPolicies) != 1 {
		t.Fatalf("cfg=%+v err=%v", cfg.TenantToolPolicies, err)
	}
}

func hasTool(defs []core.ToolDefinition, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}
