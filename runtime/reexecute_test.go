package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
)

// runToReExecute runs one agent that sees session history, calls a tool,
// gets a tool error back, and answers.
func runToReExecute(t *testing.T) (*runtime.Orchestrator, store.Store, core.RunID, runtime.Result) {
	t.Helper()
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{
			{ID: "c1", Name: "math.add", Input: json.RawMessage(`{"a":19,"b":23}`)},
			{ID: "c2", Name: "no.such.tool", Input: json.RawMessage(`{}`)},
		}},
		model.MockResponse{Content: "the sum is 42"},
	)
	orch, s, sid := setup(t, []runtime.Agent{{Name: "calc", Instructions: "Add numbers.", Tools: []string{"math.add"}, Provider: prov}})
	ctx, cancel := waitCtx()
	defer cancel()
	if err := s.AddMessage(ctx, store.MessageProjection{SessionID: sid, Role: "user", Visibility: core.VisibilityParticipant, Content: "I like sums."}); err != nil {
		t.Fatal(err)
	}
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "add", Agents: []runtime.AgentStep{{Name: "calc", Prompt: "19+23?"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	return orch, s, runID, res
}

// Acceptance: §Persistence and Replay – re-execute a run from recorded model and tool outputs.
func TestReExecuteReproducesRun(t *testing.T) {
	orch, s, runID, orig := runToReExecute(t)
	ctx, cancel := waitCtx()
	defer cancel()

	asker := core.PrincipalID("00000000-0000-0000-0000-0000000000b7")
	again, err := orch.ReExecute(ctx, core.DefaultTenant, runID, asker)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != orig.Status || !maps.Equal(again.Outputs, orig.Outputs) || again.RunID == runID {
		t.Fatalf("original=%+v re-executed=%+v", orig, again)
	}
	copyRun, err := s.GetRun(ctx, core.DefaultTenant, again.RunID)
	if err != nil || copyRun.ParentRunID != runID || copyRun.StartedBy != asker {
		t.Fatalf("copy parent=%q err=%v", copyRun.ParentRunID, err)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, again.RunID, 0, 0)
	var outputs []string
	for _, ev := range events {
		if ev.Type == core.EvToolCompleted {
			outputs = append(outputs, string(ev.Payload))
		}
	}
	if len(outputs) != 2 || !slices.Contains(outputs, `{"output":{"sum":42},"tool":"math.add"}`) {
		t.Fatalf("tool results=%v", outputs)
	}
	msgs, _ := s.ListMessages(ctx, core.DefaultTenant, copyRun.SessionID)
	if len(msgs) == 0 || msgs[0].Content != "I like sums." {
		t.Fatalf("history not copied: %+v", msgs)
	}
}

func TestReExecuteDetectsDivergence(t *testing.T) {
	orch, _, runID, _ := runToReExecute(t)
	agent := orch.Agents["calc"]
	agent.Instructions = "Add numbers carefully."
	orch.Agents["calc"] = agent

	ctx, cancel := waitCtx()
	defer cancel()
	again, err := orch.ReExecute(ctx, core.DefaultTenant, runID, core.SystemPrincipal)
	if again.Status != core.RunFailed || err == nil || !strings.Contains(again.FailureReason, model.ErrNoFixture.Error()) {
		t.Fatalf("status=%s err=%v reason=%s", again.Status, err, again.FailureReason)
	}
}

func TestReExecuteRefusesUnfinishedOrUnrecordedRuns(t *testing.T) {
	orch, s, sid := setup(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runID, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning, Workflow: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.ReExecute(ctx, core.DefaultTenant, runID, core.SystemPrincipal); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("running: err=%v", err)
	}
	if err := s.UpdateRunStatus(ctx, core.DefaultTenant, runID, core.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, []core.Event{{TenantID: core.DefaultTenant, SessionID: sid, RunID: runID, Type: core.EvModelStarted, Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.ReExecute(ctx, core.DefaultTenant, runID, core.SystemPrincipal); !errors.Is(err, runtime.ErrNotRecorded) {
		t.Fatalf("unrecorded: err=%v", err)
	}
}
