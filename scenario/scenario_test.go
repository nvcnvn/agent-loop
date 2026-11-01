// Package scenario_test runs end-to-end agent scenarios on real model output:
// replayed from testdata fixtures by default, or recorded from a live Ollama
// server with -record (see the testing skill).
package scenario_test

import (
	"context"
	"encoding/json"
	"flag"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/model/ollama"
	"github.com/nvcnvn/agent-loop/model/openai"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

var record = flag.Bool("record", false, "record the scenarios of OLLAMA_MODEL against live Ollama (OLLAMA_BASE_URL), or of LLM_MODEL against an OpenAI-compatible endpoint (LLM_BASE_URL, LLM_API_KEY), and rewrite its fixtures")

// testModel is a model with recorded fixtures in testdata/<slug>/. Every
// scenario runs once per model; vision scenarios only for vision models.
type testModel struct {
	name   string
	vision bool
	openai bool // served over an OpenAI-compatible API instead of Ollama
}

var models = []testModel{
	{name: "gemma4:e2b-it-qat", vision: true},
	{name: "qwen3-vl:2b", vision: true},
	{name: "ministral-3:3b", vision: true},
	{name: "z-ai/glm-5.3", openai: true},
}

func (m testModel) live() bool {
	if m.openai {
		return *record && os.Getenv("LLM_MODEL") == m.name
	}
	return *record && os.Getenv("OLLAMA_MODEL") == m.name
}

func slug(name string) string { return strings.NewReplacer(":", "_", "/", "_").Replace(name) }

// forEachModel runs scenario as a subtest per model. With -record, the
// subtest of OLLAMA_MODEL talks to the live model; the others replay.
func forEachModel(t *testing.T, needsVision bool, scenario func(t *testing.T, prov model.Provider, name string)) {
	if *record && !slices.ContainsFunc(models, testModel.live) {
		t.Fatalf("-record needs OLLAMA_MODEL or LLM_MODEL set to one of the models in scenario_test.go")
	}
	for _, m := range models {
		if needsVision && !m.vision {
			continue
		}
		t.Run(slug(m.name), func(t *testing.T) {
			scenario(t, provider(t, m), m.name)
		})
	}
}

func provider(t *testing.T, m testModel) model.Provider {
	t.Helper()
	name := m.name
	path := filepath.Join("testdata", slug(name), strings.Split(t.Name(), "/")[0]+".json")
	if !m.live() {
		rep, err := model.LoadReplayer(path)
		if err != nil {
			hint := "make record-host-ollama OLLAMA_MODEL=" + name
			if m.openai {
				hint = "make record-openai LLM_MODEL=" + name
			}
			t.Fatalf("%v (record it: %s)", err, hint)
		}
		t.Cleanup(func() {
			if n := rep.Remaining(); n > 0 && !t.Failed() {
				t.Errorf("%d recorded responses never requested; the agent made fewer model calls than when %s was recorded", n, path)
			}
		})
		return rep
	}
	var live model.Provider = ollama.New(os.Getenv("OLLAMA_BASE_URL"), &http.Client{Timeout: 10 * time.Minute})
	if m.openai {
		live = model.WithRetry(openai.New(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY")))
	}
	rec := model.NewRecorder(live)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("not saving %s: scenario failed against the live model", path)
			return
		}
		if err := rec.Save(path); err != nil {
			t.Error(err)
		}
	})
	return rec
}

func shapesImage(t *testing.T) map[string]model.Image {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "images", "shapes.png"))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]model.Image{"shapes": {MimeType: "image/png", Data: data}}
}

func run(t *testing.T, agents []runtime.Agent, stages ...runtime.Stage) (runtime.Result, []core.Event) {
	t.Helper()
	return newWorld(t, agents...).run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: stages}})
}

func runWithImages(t *testing.T, images map[string]model.Image, agents []runtime.Agent, stages ...runtime.Stage) (runtime.Result, []core.Event) {
	t.Helper()
	return newWorld(t, agents...).run(runtime.StartRunInput{Images: images, Workflow: runtime.Workflow{Stages: stages}})
}

// world is an orchestrator, its tools and one session that several runs share.
type world struct {
	t        *testing.T
	orch     *runtime.Orchestrator
	store    store.Store
	reg      *tool.Registry
	sid      core.SessionID
	timeout  time.Duration
	decision string // how approval requests are resolved; empty fails the run on one
}

func newWorld(t *testing.T, agents ...runtime.Agent) *world {
	t.Helper()
	s := store.NewMemStore()
	reg := tool.NewRegistry()
	if err := tool.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	sid, err := s.CreateSession(context.Background(), store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, orch: runtime.NewOrchestrator(s, reg, agents), store: s, reg: reg, sid: sid, timeout: 10 * time.Second}
	if *record {
		w.timeout = 10 * time.Minute
	}
	return w
}

// say adds a human message to the session, as POST /v1/sessions/{sid}/messages does.
func (w *world) say(text string) {
	w.t.Helper()
	if err := w.store.AddMessage(context.Background(), store.MessageProjection{SessionID: w.sid, Role: "user", Visibility: core.VisibilityParticipant, Content: text}); err != nil {
		w.t.Fatal(err)
	}
}

// run starts a run in the session, resolves its approvals with w.decision,
// and requires it to succeed.
func (w *world) run(in runtime.StartRunInput) (runtime.Result, []core.Event) {
	w.t.Helper()
	res, events := w.try(in)
	if res.Status != core.RunSucceeded {
		w.t.Fatalf("run: status=%s reason=%s", res.Status, res.FailureReason)
	}
	return res, events
}

func (w *world) try(in runtime.StartRunInput) (runtime.Result, []core.Event) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), w.timeout)
	defer cancel()
	in.TenantID, in.SessionID = core.DefaultTenant, w.sid
	in.Limits.MaxRuntime = w.timeout
	if in.Workflow.Name == "" {
		in.Workflow.Name = w.t.Name()
	}
	runID, err := w.orch.StartRun(ctx, in)
	if err != nil {
		w.t.Fatal(err)
	}
	done := make(chan struct{})
	defer close(done)
	go w.resolveApprovals(ctx, runID, done)
	res, _ := w.orch.Wait(ctx, core.DefaultTenant, runID)
	events, err := w.store.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 10000)
	if err != nil {
		w.t.Fatal(err)
	}
	if again, _ := w.orch.ReExecute(ctx, core.DefaultTenant, runID, core.SystemPrincipal); again.Status != res.Status || !maps.Equal(again.Outputs, res.Outputs) {
		w.t.Errorf("re-execution diverged: status=%s reason=%s outputs=%v, original status=%s outputs=%v", again.Status, again.FailureReason, again.Outputs, res.Status, res.Outputs)
	}
	return res, events
}

func (w *world) resolveApprovals(ctx context.Context, runID core.RunID, done <-chan struct{}) {
	seen := map[core.ApprovalID]bool{}
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
		events, _ := w.store.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 10000)
		for _, ev := range events {
			var p struct {
				ApprovalID core.ApprovalID `json:"approval_id"`
			}
			if ev.Type != core.EvApprovalReq || json.Unmarshal(ev.Payload, &p) != nil || seen[p.ApprovalID] {
				continue
			}
			seen[p.ApprovalID] = true
			decision := w.decision
			if decision == "" {
				w.t.Errorf("unexpected approval request: %s", ev.Payload)
				decision = "rejected"
			}
			_ = w.store.ResolveApproval(ctx, core.DefaultTenant, p.ApprovalID, decision, "", nil)
		}
	}
}

// toolCall is one tool call of a run, from its scheduled and completed events.
type toolCall struct {
	Tool, Input, Output, Error string
}

func toolCalls(t *testing.T, events []core.Event) []toolCall {
	t.Helper()
	var calls []toolCall
	byStep := map[core.StepID]int{}
	for _, ev := range events {
		var p struct {
			Tool   string          `json:"tool"`
			Input  json.RawMessage `json:"input"`
			Output json.RawMessage `json:"output"`
			Error  string          `json:"error"`
		}
		if (ev.Type != core.EvToolScheduled && ev.Type != core.EvToolCompleted) || json.Unmarshal(ev.Payload, &p) != nil {
			continue
		}
		if ev.Type == core.EvToolScheduled {
			byStep[ev.StepID] = len(calls)
			calls = append(calls, toolCall{Tool: p.Tool, Input: string(p.Input)})
			continue
		}
		if i, ok := byStep[ev.StepID]; ok {
			calls[i].Output, calls[i].Error = string(p.Output), p.Error
		}
	}
	return calls
}

func callsTo(calls []toolCall, name string) []toolCall {
	var out []toolCall
	for _, c := range calls {
		if c.Tool == name {
			out = append(out, c)
		}
	}
	return out
}

func toolOutputs(t *testing.T, events []core.Event) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, ev := range events {
		if ev.Type != core.EvToolCompleted {
			continue
		}
		var p struct {
			Tool   string          `json:"tool"`
			Output json.RawMessage `json:"output"`
			Error  string          `json:"error"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Error != "" {
			t.Fatalf("tool %s failed: %s", p.Tool, p.Error)
		}
		out[p.Tool] = append(out[p.Tool], string(p.Output))
	}
	return out
}

func TestPlainAnswer(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		res, _ := run(t,
			[]runtime.Agent{{Name: "assistant", Model: name, Provider: prov, Instructions: "You are terse."}},
			runtime.Stage{Name: "reply", Kind: runtime.StageSequential, Agents: []runtime.AgentStep{
				{Name: "assistant", Prompt: "Repeat this word and nothing else: pong"},
			}},
		)
		if got := strings.ToLower(res.Outputs["reply"]); !strings.Contains(got, "pong") {
			t.Fatalf("reply=%q", res.Outputs["reply"])
		}
	})
}

func TestToolCallRoundTrip(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		res, events := run(t,
			[]runtime.Agent{{
				Name: "calculator", Model: name, Provider: prov, Tools: []string{"math.add"},
				Instructions: "Always use the math.add tool for arithmetic. After the tool result, answer with only the number.",
			}},
			runtime.Stage{Name: "sum", Kind: runtime.StageSequential, Agents: []runtime.AgentStep{
				{Name: "calculator", Prompt: "What is 19 + 23?"},
			}},
		)
		if got := toolOutputs(t, events)["math.add"]; len(got) == 0 || got[0] != `{"sum":42}` {
			t.Fatalf("math.add outputs=%v", got)
		}
		if !strings.Contains(res.Outputs["sum"], "42") {
			t.Fatalf("answer=%q", res.Outputs["sum"])
		}
	})
}

func TestParallelAgentsThenSynthesis(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		toolAgent := func(agentName, toolName string) runtime.Agent {
			return runtime.Agent{
				Name: agentName, Model: name, Provider: prov, Tools: []string{toolName},
				Instructions: "Always call the " + toolName + " tool exactly once, then reply with only its result value.",
			}
		}
		res, events := run(t,
			[]runtime.Agent{
				toolAgent("shouter", "uppercase"),
				toolAgent("adder", "math.add"),
				{Name: "writer", Model: name, Provider: prov, Instructions: "You are terse."},
			},
			runtime.Stage{Name: "work", Kind: runtime.StageParallel, FailurePolicy: runtime.FailFast, Agents: []runtime.AgentStep{
				{Name: "shouter", Prompt: "Uppercase the text: agent loop"},
				{Name: "adder", Prompt: "Add 2 and 3."},
			}},
			runtime.Stage{Name: "summary", Kind: runtime.StageSequential, Agents: []runtime.AgentStep{
				{Name: "writer", Prompt: "Repeat these results on one line:\n{{stage.work}}"},
			}},
		)
		outputs := toolOutputs(t, events)
		if got := outputs["uppercase"]; len(got) == 0 || got[0] != `{"text":"AGENT LOOP"}` {
			t.Fatalf("uppercase outputs=%v", got)
		}
		if got := outputs["math.add"]; len(got) == 0 || got[0] != `{"sum":5}` {
			t.Fatalf("math.add outputs=%v", got)
		}
		if got := res.Outputs["summary"]; !strings.Contains(got, "5") || !strings.Contains(strings.ToUpper(got), "AGENT LOOP") {
			t.Fatalf("summary=%q", got)
		}
	})
}

func TestImageQuestion(t *testing.T) {
	forEachModel(t, true, func(t *testing.T, prov model.Provider, name string) {
		res, _ := runWithImages(t, shapesImage(t),
			[]runtime.Agent{{Name: "looker", Model: name, Provider: prov, Instructions: "You are terse."}},
			runtime.Stage{Name: "look", Agents: []runtime.AgentStep{
				{Name: "looker", Prompt: "What color are the circles in this image? Answer with one word.", Images: []string{"shapes"}},
			}},
		)
		if got := strings.ToLower(res.Outputs["look"]); !strings.Contains(got, "red") {
			t.Fatalf("answer=%q", res.Outputs["look"])
		}
	})
}

func TestImageCountThenTool(t *testing.T) {
	forEachModel(t, true, func(t *testing.T, prov model.Provider, name string) {
		res, events := runWithImages(t, shapesImage(t),
			[]runtime.Agent{{
				Name: "counter", Model: name, Provider: prov, Tools: []string{"math.add"},
				Instructions: "Always use the math.add tool for arithmetic. After the tool result, answer with only the number.",
			}},
			runtime.Stage{Name: "count", Agents: []runtime.AgentStep{
				{Name: "counter", Prompt: "Count the circles and count the squares in this image, then add the two counts.", Images: []string{"shapes"}},
			}},
		)
		if got := toolOutputs(t, events)["math.add"]; len(got) == 0 || got[len(got)-1] != `{"sum":5}` {
			t.Fatalf("math.add outputs=%v", got)
		}
		if !strings.Contains(res.Outputs["count"], "5") {
			t.Fatalf("answer=%q", res.Outputs["count"])
		}
	})
}
