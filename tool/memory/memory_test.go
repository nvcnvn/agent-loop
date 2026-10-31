package memory_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
	"github.com/nvcnvn/agent-loop/tool/memory"
)

type fixture struct {
	t   *testing.T
	s   *store.MemStore
	reg *tool.Registry
}

func newFixture(t *testing.T, scope store.MemoryScope) fixture {
	s := store.NewMemStore()
	reg := tool.NewRegistry()
	if err := (memory.Tools{Store: s, Embedder: model.NewMockProvider(), Model: "bow", WriteScope: scope}).Register(reg); err != nil {
		t.Fatal(err)
	}
	return fixture{t: t, s: s, reg: reg}
}

// run creates a run of workflow in a new session and returns a call context for it.
func (f fixture) run(workflow string) tool.Call {
	f.t.Helper()
	ctx := context.Background()
	sid, _ := f.s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	rid, err := f.s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning, Workflow: json.RawMessage(`{"name":"` + workflow + `"}`)})
	if err != nil {
		f.t.Fatal(err)
	}
	return tool.Call{TenantID: core.DefaultTenant, RunID: rid, TaskID: core.TaskID(core.NewID()), StepID: core.StepID(core.NewID())}
}

func (f fixture) call(c tool.Call, name, input string) (string, error) {
	f.t.Helper()
	_, exec, ok := f.reg.Get(name)
	if !ok {
		f.t.Fatalf("%s not registered", name)
	}
	c.Input = json.RawMessage(input)
	res, err := exec.Execute(context.Background(), c)
	return string(res.Output), err
}

func TestSaveThenSearchAcrossSessions(t *testing.T) {
	f := newFixture(t, store.MemoryTenant)
	first := f.run("support")
	if _, err := f.call(first, memory.SaveTool, `{"content":"Linh's dog is called Pixel"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(first, memory.SaveTool, `{"content":"The office closes at six"}`); err != nil {
		t.Fatal(err)
	}

	out, err := f.call(f.run("support"), memory.SearchTool, `{"query":"what is the dog called"}`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Memories []struct {
			Content string             `json:"content"`
			Source  store.MemorySource `json:"source"`
		} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Memories) != 2 {
		t.Fatalf("search output %s err=%v", out, err)
	}
	if top := res.Memories[0]; top.Content != "Linh's dog is called Pixel" || top.Source.RunID != first.RunID || top.Source.TaskID != first.TaskID || top.Source.SessionID == "" {
		t.Fatalf("top memory %+v, want the dog with provenance of run %s", top, first.RunID)
	}
}

func TestWriteScopeLimitsWhoFindsAMemory(t *testing.T) {
	for _, c := range []struct {
		scope         store.MemoryScope
		otherSession  bool // found from another session of the same workflow
		otherWorkflow bool // found from a run of another workflow
	}{
		{store.MemorySession, false, false},
		{store.MemoryWorkflow, true, false},
		{store.MemoryTenant, true, true},
	} {
		f := newFixture(t, c.scope)
		if _, err := f.call(f.run("support"), memory.SaveTool, `{"content":"refunds need a receipt"}`); err != nil {
			t.Fatal(err)
		}
		found := func(call tool.Call) bool {
			out, err := f.call(call, memory.SearchTool, `{"query":"refunds receipt"}`)
			if err != nil {
				t.Fatal(err)
			}
			return strings.Contains(out, "receipt")
		}
		if found(f.run("support")) != c.otherSession || found(f.run("sales")) != c.otherWorkflow {
			t.Errorf("scope %s: other session %v, other workflow %v", c.scope, found(f.run("support")), found(f.run("sales")))
		}
	}
}

func TestSaveRejectsEmptyAndOversizedMemories(t *testing.T) {
	f := newFixture(t, "")
	c := f.run("w")
	for _, input := range []string{`{"content":"  "}`, `{"content":"` + strings.Repeat("x", 2001) + `"}`, `{"content":7}`, `"just text"`} {
		if _, err := f.call(c, memory.SaveTool, input); err == nil {
			t.Errorf("saved %.40s", input)
		}
	}
	if hits, _ := f.s.SearchMemories(context.Background(), store.MemoryQuery{TenantID: core.DefaultTenant, EmbedModel: "bow", Embedding: make([]float32, 64)}); len(hits) != 0 {
		t.Fatalf("stored %d memories", len(hits))
	}
}

func FuzzToolInput(f *testing.F) {
	f.Add(`{"content":"a fact"}`)
	f.Add(`{"query":""}`)
	f.Add(`{"content":null,"query":[1]}`)
	f.Fuzz(func(t *testing.T, input string) {
		fx := newFixture(t, store.MemorySession)
		c := fx.run("w")
		_, _ = fx.call(c, memory.SaveTool, input)
		_, _ = fx.call(c, memory.SearchTool, input)
	})
}
