package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

// ErrNotRecorded is returned when a run's events lack what re-execution needs.
var ErrNotRecorded = errors.New("run was not recorded for re-execution")

// ReExecute runs a finished run again from its stored events and waits for the
// copy to finish. The copy has the original's workflow, input, images and
// limits, and runs as its child (ParentRunID) in a new session holding the
// history the original saw. Each model call is answered with the response
// recorded for an identical request, and each tool call with the result
// recorded for the same tool and input; approvals are not asked again. The
// copy ends like the original unless the runtime now builds different
// requests or handles the same responses differently, and a request that was
// never recorded fails it with model.ErrNoFixture. by is who asked, the copy's starter.
func (o *Orchestrator) ReExecute(ctx context.Context, tenant core.TenantID, runID core.RunID, by core.PrincipalID) (Result, error) {
	run, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		return Result{}, err
	}
	if !run.Status.Terminal() {
		return Result{}, fmt.Errorf("run %s is %s: %w", runID, run.Status, core.ErrConflict)
	}
	events, err := o.Store.ListRunEvents(ctx, tenant, runID, 0, 0)
	if err != nil {
		return Result{}, err
	}
	replayer, tools, err := recordedOutputs(events)
	if err != nil {
		return Result{}, err
	}

	in := StartRunInput{TenantID: tenant, PrincipalID: by, Limits: run.Limits, parent: runID}
	if err := json.Unmarshal(run.Workflow, &in.Workflow); err != nil {
		return Result{}, fmt.Errorf("stored workflow: %w", err)
	}
	if len(run.Input) > 0 {
		if err := json.Unmarshal(run.Input, &in.Input); err != nil {
			return Result{}, fmt.Errorf("stored input: %w", err)
		}
	}
	artifacts, err := o.Store.ListRunArtifacts(ctx, tenant, runID)
	if err != nil {
		return Result{}, err
	}
	for _, a := range artifacts {
		if a.Kind == inputImageKind && a.Producer == "" {
			if in.Images == nil {
				in.Images = map[string]model.Image{}
			}
			in.Images[a.Name] = model.Image{MimeType: a.MimeType, Data: a.Content}
		}
	}
	if in.SessionID, err = o.copyHistory(ctx, run); err != nil {
		return Result{}, err
	}

	c, err := o.replayOrchestrator(replayer, tools)
	if err != nil {
		return Result{}, err
	}
	copyID, err := c.StartRun(ctx, in)
	if err != nil {
		return Result{}, err
	}
	return c.Wait(ctx, tenant, copyID)
}

// recordedOutputs collects the run's model exchanges and tool results.
func recordedOutputs(events []core.Event) (*model.Replayer, *toolReplay, error) {
	requests := map[core.StepID]model.ChatRequest{}
	toolInputs := map[core.StepID]string{}
	var exchanges []model.Exchange
	tools := &toolReplay{results: map[string][]toolResult{}}
	for _, ev := range events {
		switch ev.Type {
		case core.EvModelStarted:
			var h struct {
				Request *model.ChatRequest `json:"request"`
			}
			if err := json.Unmarshal(ev.HiddenPayload, &h); err != nil || h.Request == nil {
				return nil, nil, fmt.Errorf("%w: model call %s has no stored request", ErrNotRecorded, ev.StepID)
			}
			requests[ev.StepID] = *h.Request
		case core.EvModelCompleted:
			var h struct {
				Response *model.ChatResponse `json:"response"`
			}
			if err := json.Unmarshal(ev.HiddenPayload, &h); err != nil || h.Response == nil {
				return nil, nil, fmt.Errorf("%w: model call %s has no stored response", ErrNotRecorded, ev.StepID)
			}
			exchanges = append(exchanges, model.Exchange{Request: requests[ev.StepID], Response: *h.Response})
		case core.EvToolScheduled:
			var p struct {
				Tool  string          `json:"tool"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, nil, fmt.Errorf("tool call %s: %w", ev.StepID, err)
			}
			toolInputs[ev.StepID] = toolKey(p.Tool, p.Input)
		case core.EvToolCompleted:
			var p struct {
				Output    json.RawMessage `json:"output"`
				Error     string          `json:"error"`
				Fatal     string          `json:"fatal"`
				CostUnits int64           `json:"cost_units"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, nil, fmt.Errorf("tool call %s: %w", ev.StepID, err)
			}
			if key, ok := toolInputs[ev.StepID]; ok {
				tools.results[key] = append(tools.results[key], toolResult{output: p.Output, err: p.Error, fatal: p.Fatal, cost: p.CostUnits})
			}
		}
	}
	replayer, err := model.NewReplayer(exchanges)
	return replayer, tools, err
}

type toolResult struct {
	output json.RawMessage
	err    string
	fatal  string // the code of an error that failed the task
	cost   int64
}

// toolReplay answers tool calls with recorded results, in recorded order per
// tool and input.
type toolReplay struct {
	mu      sync.Mutex
	results map[string][]toolResult
}

func toolKey(name string, input json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, input) != nil {
		b.Reset()
		b.Write(input)
	}
	return name + "\x00" + b.String()
}

func (t *toolReplay) Execute(_ context.Context, c tool.Call) (tool.Result, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := toolKey(c.Tool.Name, c.Input)
	q := t.results[key]
	if len(q) == 0 {
		return tool.Result{}, fmt.Errorf("%w: no recorded result for tool %s with input %s", ErrNotRecorded, c.Tool.Name, c.Input)
	}
	t.results[key] = q[1:]
	r := q[0]
	switch {
	case r.fatal == "budget_exceeded":
		return tool.Result{}, fmt.Errorf("%w: %s", core.ErrBudgetExceeded, r.err)
	case r.fatal == "loop_detected", r.fatal == "paused":
		return tool.Result{}, fmt.Errorf("%w: %s", errLoopDetected, r.err)
	case r.err != "":
		return tool.Result{}, errors.New(r.err)
	}
	return tool.Result{Output: r.output, CostUnits: r.cost}, nil
}

// replayOrchestrator is a copy of o whose agents and tools answer from records.
func (o *Orchestrator) replayOrchestrator(p model.Provider, tools tool.Executor) (*Orchestrator, error) {
	reg := tool.NewRegistry()
	for _, def := range o.Tools.List() {
		if err := reg.Register(def, tools); err != nil {
			return nil, err
		}
	}
	agents := make([]Agent, 0, len(o.Agents))
	for _, a := range o.Agents {
		a.Provider = p
		agents = append(agents, a)
	}
	c := NewOrchestrator(o.Store, reg, agents)
	c.Workflows, c.MaxLimits, c.MaxHistoryMessages, c.Concurrency, c.Compaction = o.Workflows, o.MaxLimits, o.MaxHistoryMessages, o.Concurrency, o.Compaction
	c.Policy, c.skipApprovals = o.Policy, true
	c.Log, c.Tracer = o.Log, o.Tracer
	return c, nil
}

// copyHistory creates a session holding the messages the run's agents saw:
// those of its session from before the run started, except its own.
func (o *Orchestrator) copyHistory(ctx context.Context, run store.Run) (core.SessionID, error) {
	orig, err := o.Store.GetSession(ctx, run.TenantID, run.SessionID)
	if err != nil {
		return "", err
	}
	sid, err := o.Store.CreateSession(ctx, store.Session{TenantID: run.TenantID, CreatedBy: orig.CreatedBy, Title: "re-execution of run " + string(run.ID)})
	if err != nil {
		return "", err
	}
	msgs, err := o.Store.ListMessages(ctx, run.TenantID, run.SessionID)
	if err != nil {
		return "", err
	}
	copied := map[string]string{}
	for _, m := range msgs {
		if m.RunID == run.ID || !m.CreatedAt.Before(run.StartedAt) {
			continue
		}
		copied[m.ID] = core.NewID()
		m.ID, m.SessionID, m.EventID = copied[m.ID], sid, ""
		if m.Role == snapshotRole {
			var s snapshot
			if json.Unmarshal([]byte(m.Content), &s) == nil && s.ThroughID != "" {
				s.ThroughID = copied[s.ThroughID]
				m.Content = string(mustJSON(s))
			}
		}
		if err := o.Store.AddMessage(ctx, m); err != nil {
			return "", err
		}
	}
	return sid, nil
}
