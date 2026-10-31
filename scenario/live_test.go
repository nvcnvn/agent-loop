package scenario_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/model/ollama"
	"github.com/nvcnvn/agent-loop/model/openai"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool/memory"
)

// liveProvider returns the .env endpoint, skipping unless paid tests are allowed.
func liveProvider(t *testing.T) (model.Provider, string) {
	t.Helper()
	if os.Getenv("ALLOW_PAID_LLM") != "1" || os.Getenv("LLM_BASE_URL") == "" {
		t.Skip("paid: set ALLOW_PAID_LLM=1 and LLM_* (make test-paid)")
	}
	return model.WithRetry(openai.New(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"))), os.Getenv("LLM_MODEL")
}

// chatter adds n human messages of about 100 tokens each that bury earlier facts.
func (w *world) chatter(topic string, n int) {
	for i := range n {
		w.say(fmt.Sprintf("Unrelated note %d about %s: the morning was grey and then bright, the bus came "+
			"eleven minutes late, a neighbour repainted a fence in a pale green, the market had more mangoes "+
			"than usual, and somebody's radio played the same song %d times before noon. Nothing to act on.", i, topic, i%7+2))
	}
}

// TestLiveLongSessionCompaction runs a long multi-turn session on a hosted
// model with compaction on, and checks that facts from early turns survive
// two summaries while the answering call's prompt stays small.
// About 15k tokens in total.
func TestLiveLongSessionCompaction(t *testing.T) {
	prov, name := liveProvider(t)
	w := newWorld(t,
		runtime.Agent{Name: "summarizer", Model: name, Provider: prov, Instructions: "You condense support conversations without losing customer details."},
		runtime.Agent{Name: "assistant", Model: name, Provider: prov, Instructions: "You are a support assistant. Answer in one short line."},
	)
	w.timeout = 5 * time.Minute
	w.orch.Compaction = runtime.Compaction{Agent: "summarizer", MaxTokens: 2000, KeepRecent: 4}

	ask := func(question string) (string, []core.Event) {
		t.Helper()
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "answer", Agents: []runtime.AgentStep{{Name: "assistant", Prompt: question}}},
		}}})
		return res.Outputs["answer"], events
	}
	check := func(answer string, events []core.Event, wantSnapshot bool, facts ...string) {
		t.Helper()
		for _, f := range facts {
			if !strings.Contains(strings.ToLower(answer), strings.ToLower(f)) {
				t.Errorf("answer %q lacks %q", answer, f)
			}
		}
		snapshots, maxPrompt := 0, int64(0)
		for _, ev := range events {
			switch ev.Type {
			case core.EvSnapshot:
				snapshots++
			case core.EvModelCompleted:
				var p struct {
					Usage core.Usage `json:"usage"`
				}
				if ev.TaskID != "" && json.Unmarshal(ev.Payload, &p) == nil {
					maxPrompt = max(maxPrompt, p.Usage.PromptTokens)
				}
			}
		}
		if (snapshots == 1) != wantSnapshot {
			t.Errorf("snapshots=%d want %v", snapshots, wantSnapshot)
		}
		if maxPrompt > 2000 {
			t.Errorf("assistant prompt was %d tokens; compaction did not shrink it", maxPrompt)
		}
		t.Logf("answer=%q assistant prompt tokens=%d", answer, maxPrompt)
	}

	w.say("Hi, I'm Linh. My account number is 88-4413.")
	w.say("My dog is called Pixel, he chewed the router cable.")
	w.say("I'm allergic to peanuts, so no snack vouchers please.")
	w.chatter("the weather", 35)
	answer, events := ask("What is my account number, my dog's name, and my allergy?")
	check(answer, events, true, "88-4413", "Pixel", "peanut")

	w.say("I moved: my new address is 12 Le Loi street.")
	w.chatter("the commute", 35)
	answer, events = ask("Remind me of my account number and my new address.")
	check(answer, events, true, "88-4413", "Le Loi")

	answer, events = ask("And my dog's name?")
	check(answer, events, false, "Pixel")
}

// TestLiveMemoryAcrossSessions has a hosted model save customer facts in one
// session and recall them in another, which shares no history, through the
// memory tools over Ollama embeddings (OLLAMA_EMBED_MODEL at OLLAMA_BASE_URL).
func TestLiveMemoryAcrossSessions(t *testing.T) {
	prov, name := liveProvider(t)
	embedModel := os.Getenv("OLLAMA_EMBED_MODEL")
	if embedModel == "" {
		t.Skip("set OLLAMA_EMBED_MODEL (and OLLAMA_BASE_URL) for embeddings")
	}
	w := newWorld(t, runtime.Agent{
		Name: "assistant", Model: name, Provider: prov, MaxToolLoops: 6,
		Tools: []string{memory.SaveTool, memory.SearchTool},
		Instructions: "You are a support assistant with long-term memory. Save each lasting customer fact " +
			"(name, pets, allergies, preferences) with memory.save, one fact per call. Before answering a " +
			"question about the customer, call memory.search. Answer in one short line.",
	})
	w.timeout = 5 * time.Minute
	tools := memory.Tools{Store: w.store, Embedder: ollama.New(os.Getenv("OLLAMA_BASE_URL"), nil), Model: embedModel, WriteScope: store.MemoryTenant}
	if err := tools.Register(w.reg); err != nil {
		t.Fatal(err)
	}
	ask := func(prompt string) (string, []toolCall) {
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Name: "support", Stages: []runtime.Stage{
			{Name: "reply", Agents: []runtime.AgentStep{{Name: "assistant", Prompt: prompt}}},
		}}})
		return res.Outputs["reply"], toolCalls(t, events)
	}

	_, calls := ask("Customer: Hi, I'm Linh. My dog Pixel chewed the router cable, and I'm allergic to peanuts.")
	saves := 0
	for _, c := range calls {
		if c.Tool == memory.SaveTool && c.Error == "" {
			saves++
		}
	}
	if saves == 0 {
		t.Fatalf("nothing saved: %+v", calls)
	}

	sid, err := w.store.CreateSession(context.Background(), store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	w.sid = sid
	answer, calls := ask("Customer Linh is back. What is her dog called, and what is she allergic to?")
	searched := false
	for _, c := range calls {
		searched = searched || c.Tool == memory.SearchTool && strings.Contains(c.Output, "Pixel")
	}
	if !searched || !strings.Contains(answer, "Pixel") || !strings.Contains(strings.ToLower(answer), "peanut") {
		t.Fatalf("answer=%q calls=%+v", answer, calls)
	}
	t.Logf("saved %d memories; answer=%q", saves, answer)
}
