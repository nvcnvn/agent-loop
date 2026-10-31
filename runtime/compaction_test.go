package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
)

type compactionWorld struct {
	t      *testing.T
	orch   *runtime.Orchestrator
	store  store.Store
	sid    core.SessionID
	sum    *model.MockProvider
	answer *model.MockProvider
}

func newCompactionWorld(t *testing.T, c runtime.Compaction) *compactionWorld {
	w := &compactionWorld{t: t, sum: model.NewMockProvider(), answer: model.NewMockProvider()}
	w.orch, w.store, w.sid = setup(t, []runtime.Agent{
		{Name: "summarizer", Instructions: "You summarize.", Provider: w.sum},
		{Name: "main", Provider: w.answer},
	})
	c.Agent = "summarizer"
	w.orch.Compaction = c
	return w
}

func (w *compactionWorld) say(texts ...string) {
	for _, text := range texts {
		if err := w.store.AddMessage(context.Background(), store.MessageProjection{SessionID: w.sid, Role: "user", Visibility: core.VisibilityParticipant, Content: text}); err != nil {
			w.t.Fatal(err)
		}
	}
}

// ask runs main once and returns the messages its model call saw and the run.
func (w *compactionWorld) ask(prompt string) ([]model.Message, core.RunID) {
	w.t.Helper()
	w.answer.Push(model.MockResponse{Content: "answer to " + prompt})
	ctx, cancel := waitCtx()
	defer cancel()
	runID, err := w.orch.StartRun(ctx, runtime.StartRunInput{SessionID: w.sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "main", Prompt: prompt}}},
	}}})
	if err != nil {
		w.t.Fatal(err)
	}
	if res, err := w.orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		w.t.Fatalf("run: %+v %v", res, err)
	}
	calls := w.answer.Calls()
	return calls[len(calls)-1].Messages, runID
}

func contents(msgs []model.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role + ": " + m.Content
	}
	return out
}

// Acceptance: §Snapshot and Compaction #1, #3, #4.
func TestCompactionSummarizesOlderHistory(t *testing.T) {
	w := newCompactionWorld(t, runtime.Compaction{MaxMessages: 4, KeepRecent: 2})
	w.say("my name is Ada", "I live in Hue", "my order is 1042", "it is late", "where is it?", "please hurry")
	w.sum.Push(model.MockResponse{Content: "Ada from Hue; order 1042 is late."})

	seen, runID := w.ask("status?")
	want := []string{
		"user: Summary of the earlier conversation:\nAda from Hue; order 1042 is late.",
		"user: where is it?",
		"user: please hurry",
		"user: status?",
	}
	if got := contents(seen); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("main saw:\n%q\nwant:\n%q", got, want)
	}
	sumReq := w.sum.Calls()[0].Messages
	if sumReq[0].Content != "You summarize." || !strings.Contains(sumReq[1].Content, "[user] my name is Ada\n[user] I live in Hue\n[user] my order is 1042\n[user] it is late\n") {
		t.Fatalf("summarizer request: %q", contents(sumReq))
	}

	ctx := context.Background()
	msgs, _ := w.store.ListMessages(ctx, core.DefaultTenant, w.sid)
	raw := 0
	for _, m := range msgs {
		if m.Role == "user" {
			raw++
		}
	}
	if raw != 6 {
		t.Fatalf("raw history lost: %d user messages left", raw)
	}
	events, _ := w.store.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	var snaps, modelCalls int
	for _, ev := range events {
		switch ev.Type {
		case core.EvSnapshot:
			snaps++
			if !strings.Contains(string(ev.Payload), `"summarized":4`) || !strings.Contains(string(ev.HiddenPayload), "order 1042") {
				t.Fatalf("snapshot event %s %s", ev.Payload, ev.HiddenPayload)
			}
		case core.EvModelCompleted:
			modelCalls++
		}
	}
	if snaps != 1 || modelCalls != 2 {
		t.Fatalf("snapshots=%d model calls=%d", snaps, modelCalls)
	}

	again, err := w.orch.ReExecute(ctx, core.DefaultTenant, runID)
	if err != nil || again.Outputs["s"] != "answer to status?" {
		t.Fatalf("re-execute: %+v %v", again, err)
	}
}

func TestCompactionFoldsTheLastSummaryIntoTheNext(t *testing.T) {
	w := newCompactionWorld(t, runtime.Compaction{MaxMessages: 3, KeepRecent: 1})
	w.say("a1", "a2", "a3", "a4")
	w.sum.Push(model.MockResponse{Content: "S1"})
	w.ask("q1")

	seen, _ := w.ask("q2")
	if got := contents(seen); got[0] != "user: Summary of the earlier conversation:\nS1" || len(got) != 4 {
		t.Fatalf("below the threshold main should see S1 and the tail: %q", got)
	}
	if len(w.sum.Calls()) != 1 {
		t.Fatalf("summarized again below the threshold")
	}

	w.say("b1", "b2")
	w.sum.Push(model.MockResponse{Content: "S2"})
	seen, _ = w.ask("q3")
	prompt := w.sum.Calls()[1].Messages[1].Content
	if !strings.HasPrefix(prompt, "Summary so far:\nS1\n\nConversation since then:\n[user] a4\n[assistant] answer to q1\n") {
		t.Fatalf("second summary prompt: %q", prompt)
	}
	if got := contents(seen); got[0] != "user: Summary of the earlier conversation:\nS2" || got[1] != "user: b2" {
		t.Fatalf("main saw %q", got)
	}
}

// Acceptance: §Snapshot and Compaction #2.
func TestCompactionByTokens(t *testing.T) {
	w := newCompactionWorld(t, runtime.Compaction{MaxTokens: 100})
	w.say(strings.Repeat("long words ", 20))
	seen, _ := w.ask("short")
	if len(w.sum.Calls()) != 0 || len(seen) != 2 {
		t.Fatalf("compacted below the token threshold: %q", contents(seen))
	}
	w.say(strings.Repeat("more words ", 30))
	w.sum.Push(model.MockResponse{Content: "S"})
	seen, _ = w.ask("again")
	if len(w.sum.Calls()) != 1 || fmt.Sprint(contents(seen)) != "[user: Summary of the earlier conversation:\nS user: again]" {
		t.Fatalf("main saw %q", contents(seen))
	}
}
