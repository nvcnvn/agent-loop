// Package scenario_test runs end-to-end agent scenarios on real model output:
// replayed from testdata fixtures by default, or recorded from a live Ollama
// server with -record (see the testing skill).
package scenario_test

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/model/ollama"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

var record = flag.Bool("record", false, "run against live Ollama (OLLAMA_BASE_URL, OLLAMA_MODEL) and rewrite testdata fixtures")

const defaultModel = "gemma4:e2b-it-qat"

func provider(t *testing.T) (model.Provider, string) {
	t.Helper()
	path := filepath.Join("testdata", t.Name()+".json")
	if !*record {
		rep, err := model.LoadReplayer(path)
		if err != nil {
			t.Fatalf("%v (record it: make record-docker-ollama)", err)
		}
		t.Cleanup(func() {
			if n := rep.Remaining(); n > 0 && !t.Failed() {
				t.Errorf("%d recorded responses never requested; the agent made fewer model calls than when %s was recorded", n, path)
			}
		})
		return rep, defaultModel
	}
	name := os.Getenv("OLLAMA_MODEL")
	if name == "" {
		name = defaultModel
	}
	rec := model.NewRecorder(ollama.New(os.Getenv("OLLAMA_BASE_URL"), &http.Client{Timeout: 10 * time.Minute}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("not saving %s: scenario failed against the live model", path)
			return
		}
		if err := rec.Save(path); err != nil {
			t.Error(err)
		}
	})
	return rec, name
}

func run(t *testing.T, agents []runtime.Agent, stages ...runtime.Stage) (runtime.Result, []core.Event) {
	t.Helper()
	s := store.NewMemStore()
	reg := tool.NewRegistry()
	if err := tool.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	orch := runtime.NewOrchestrator(s, reg, agents)
	timeout := 10 * time.Second
	if *record {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	sid, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		TenantID:  core.DefaultTenant,
		SessionID: sid,
		Workflow:  runtime.Workflow{Name: t.Name(), Stages: stages},
		Limits:    core.Limits{MaxRuntime: timeout},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("run: err=%v status=%s reason=%s", err, res.Status, res.FailureReason)
	}
	events, err := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return res, events
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
	prov, name := provider(t)
	res, _ := run(t,
		[]runtime.Agent{{Name: "assistant", Model: name, Provider: prov, Instructions: "You are terse."}},
		runtime.Stage{Name: "reply", Kind: runtime.StageSequential, Agents: []runtime.AgentStep{
			{Name: "assistant", Prompt: "Reply with exactly one word: pong"},
		}},
	)
	if got := strings.ToLower(res.Outputs["reply"]); !strings.Contains(got, "pong") {
		t.Fatalf("reply=%q", res.Outputs["reply"])
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	prov, name := provider(t)
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
}

func TestParallelAgentsThenSynthesis(t *testing.T) {
	prov, name := provider(t)
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
}
