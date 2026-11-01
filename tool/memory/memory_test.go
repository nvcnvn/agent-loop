package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
func (f fixture) run(workflow string) tool.Call { return f.runIn("", workflow) }

// runIn is run in a session of project.
func (f fixture) runIn(project, workflow string) tool.Call {
	f.t.Helper()
	ctx := context.Background()
	sid, _ := f.s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal, Project: project})
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

// Acceptance: §Memory and Retrieval #6 – project memory reaches every session of the project only.
func TestProjectScope(t *testing.T) {
	f := newFixture(t, store.MemoryProject)
	if _, err := f.call(f.runIn("apollo", "w"), memory.SaveTool, `{"content":"The launch window opens on Tuesday"}`); err != nil {
		t.Fatal(err)
	}
	for project, want := range map[string]bool{"apollo": true, "zeus": false, "": false} {
		out, err := f.call(f.runIn(project, "w"), memory.SearchTool, `{"query":"when does the launch window open"}`)
		if err != nil || strings.Contains(out, "Tuesday") != want {
			t.Fatalf("project %q found it=%v, want %v: %s %v", project, !want, want, out, err)
		}
	}
	if _, err := f.call(f.run("w"), memory.SaveTool, `{"content":"no project here"}`); err == nil || !strings.Contains(err.Error(), "no project") {
		t.Fatalf("saving project memory outside a project: %v", err)
	}
}

// Acceptance: §Memory and Retrieval #7 – a document is ingested in parts, each with its provenance.
func TestIngestDocument(t *testing.T) {
	s := store.NewMemStore()
	tools := memory.Tools{Store: s, Embedder: model.NewMockProvider(), Model: "bow", MaxChars: 60}
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal, Project: "apollo"})
	doc := "Refunds are paid within five days.\n\nThe warehouse is in Da Nang and ships every weekday morning.\n\nSupport answers in Vietnamese and English."
	ids, err := tools.Ingest(ctx, core.DefaultTenant, sid, store.MemoryProject, doc, store.MemorySource{Document: "handbook.md", By: core.DefaultPrincipal})
	if err != nil || len(ids) != 3 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	vec, _ := model.NewMockProvider().Embed(ctx, "bow", []string{"where is the warehouse"})
	hits, err := s.SearchMemories(ctx, store.MemoryQuery{TenantID: core.DefaultTenant, Embedding: vec[0], EmbedModel: "bow", Project: "apollo", Limit: 1})
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Content, "Da Nang") {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if src := hits[0].Source; src.Document != "handbook.md" || src.Part != 2 || src.By != core.DefaultPrincipal || src.SessionID != sid {
		t.Fatalf("provenance=%+v", src)
	}
	if _, err := tools.Ingest(ctx, core.DefaultTenant, sid, store.MemoryWorkflow, doc, store.MemorySource{}); err == nil {
		t.Fatal("ingested into workflow memory")
	}
	if _, err := tools.Ingest(ctx, core.DefaultTenant, sid, store.MemorySession, " \n\n ", store.MemorySource{}); err == nil {
		t.Fatal("ingested an empty document")
	}
}

type failingEmbedder struct {
	calls int
	model.MockProvider
}

func (f *failingEmbedder) Embed(ctx context.Context, name string, texts []string) ([][]float32, error) {
	if f.calls++; f.calls > 1 {
		return nil, errors.New("embedder down")
	}
	return f.MockProvider.Embed(ctx, name, texts)
}

func TestIngestIsAllOrNothing(t *testing.T) {
	s := store.NewMemStore()
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	var doc strings.Builder
	for i := range 40 {
		fmt.Fprintf(&doc, "Paragraph %d of the handbook.\n\n", i+1)
	}
	tools := memory.Tools{Store: s, Embedder: model.NewMockProvider(), Model: "bow", MaxChars: 30}
	ids, err := tools.Ingest(ctx, core.DefaultTenant, sid, store.MemorySession, doc.String(), store.MemorySource{Document: "handbook.md"})
	if err != nil || len(ids) != 40 {
		t.Fatalf("%d parts, err=%v", len(ids), err)
	}
	vec, _ := model.NewMockProvider().Embed(ctx, "bow", []string{"paragraph 40 handbook"})
	hits, _ := s.SearchMemories(ctx, store.MemoryQuery{TenantID: core.DefaultTenant, Embedding: vec[0], EmbedModel: "bow", SessionID: sid, Limit: 1})
	if len(hits) != 1 || hits[0].Source.Part != 40 {
		t.Fatalf("hits=%+v", hits)
	}

	other, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	tools.Embedder = &failingEmbedder{}
	if _, err := tools.Ingest(ctx, core.DefaultTenant, other, store.MemorySession, doc.String(), store.MemorySource{}); err == nil {
		t.Fatal("ingest succeeded with the embedder down")
	}
	if hits, _ := s.SearchMemories(ctx, store.MemoryQuery{TenantID: core.DefaultTenant, Embedding: vec[0], EmbedModel: "bow", SessionID: other}); len(hits) != 0 {
		t.Fatalf("a failed ingest left %d memories", len(hits))
	}
	tools.MaxIngestBytes = 10
	if _, err := tools.Ingest(ctx, core.DefaultTenant, other, store.MemorySession, doc.String(), store.MemorySource{}); !errors.Is(err, store.ErrMemoryTooLarge) {
		t.Fatalf("oversized document: %v", err)
	}
}
