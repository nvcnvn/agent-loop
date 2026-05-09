// Package runtime contains the orchestrator that turns a workflow definition
// into a tree of tasks, model calls and tool calls, persisting every event.
//
// The orchestrator is event-sourced: durable state lives in the EventStore,
// and the runs/tasks tables are projections that can be rebuilt from events.
// Concurrent execution is supported via parallel task groups; events are
// always given a deterministic per-run sequence by the event store, even
// when the producing goroutines finish out of order.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

// Agent is a runtime-resolvable agent definition.
type Agent struct {
	Name         string
	Role         string
	Instructions string
	Model        string         // provider model name
	Provider     model.Provider // provider implementation
	Tools        []string       // tool names from the tool registry the agent may call
	MaxToolLoops int            // max tool-call iterations per RunAgent invocation; default 4
}

// StageKind picks between sequential and parallel execution within a workflow.
type StageKind string

const (
	StageSequential StageKind = "sequential"
	StageParallel   StageKind = "parallel"
)

// FailurePolicy mirrors the values described in the domain model for fan-in.
type FailurePolicy string

const (
	FailFast   FailurePolicy = "fail_fast"
	WaitAll    FailurePolicy = "wait_all"
	BestEffort FailurePolicy = "best_effort"
)

// AgentStep is one agent invocation inside a stage.
type AgentStep struct {
	Name   string // agent name; resolved against the registered agents
	Prompt string // prompt template; {{stage.<name>}} placeholders are filled from previous stage outputs
}

// Stage is one step of a workflow.
type Stage struct {
	Name          string
	Kind          StageKind
	Agents        []AgentStep
	FailurePolicy FailurePolicy
}

// Workflow defines an ordered list of stages.
type Workflow struct {
	Name   string
	Stages []Stage
}

// StartRunInput carries everything the orchestrator needs to begin.
type StartRunInput struct {
	TenantID       core.TenantID
	PrincipalID    core.PrincipalID
	SessionID      core.SessionID
	IdempotencyKey string
	Workflow       Workflow
	Input          map[string]string // available as {{input.<key>}} in templates
	Limits         core.Limits
	Metadata       map[string]any
}

// Result is the terminal state returned by Wait.
type Result struct {
	RunID         core.RunID
	Status        core.RunStatus
	Outputs       map[string]string // stage name -> aggregated output
	Usage         core.Usage
	FailureReason string
}

// Orchestrator is the central runtime entry point.
type Orchestrator struct {
	Store     store.Store // any driver: store.MemStore or store/pg.Store
	Tools     *tool.Registry
	Agents    map[string]Agent
	Policy    policy.ToolPolicy
	Budget    *policy.BudgetManager
	LoopDet   *policy.LoopDetector
	Approvals store.ApprovalStore // typically the same Store

	mu   sync.Mutex
	runs map[core.RunID]*runState
}

type runState struct {
	cancel  context.CancelFunc
	done    chan struct{}
	result  *Result
	err     error
	outputs map[string]string
}

// NewOrchestrator wires sensible defaults around the provided store.
func NewOrchestrator(s store.Store, reg *tool.Registry, agents []Agent) *Orchestrator {
	am := map[string]Agent{}
	for _, a := range agents {
		if a.MaxToolLoops == 0 {
			a.MaxToolLoops = 4
		}
		am[a.Name] = a
	}
	return &Orchestrator{
		Store:     s,
		Tools:     reg,
		Agents:    am,
		Budget:    policy.NewBudgetManager(),
		LoopDet:   policy.NewLoopDetector(5),
		Approvals: s,
		runs:      map[core.RunID]*runState{},
	}
}

// StartRun creates a run record and begins executing the workflow in a
// background goroutine. It returns immediately.
func (o *Orchestrator) StartRun(ctx context.Context, in StartRunInput) (core.RunID, error) {
	if in.TenantID == "" {
		in.TenantID = core.DefaultTenant
	}
	if in.PrincipalID == "" {
		in.PrincipalID = core.DefaultPrincipal
	}
	if _, err := o.Store.GetSession(ctx, in.TenantID, in.SessionID); err != nil {
		return "", fmt.Errorf("session: %w", err)
	}

	if in.IdempotencyKey != "" {
		if existing, err := o.Store.FindByIdempotency(ctx, in.TenantID, in.IdempotencyKey); err == nil {
			return existing.ID, nil
		}
	}

	wfRaw, _ := json.Marshal(in.Workflow)
	inRaw, _ := json.Marshal(in.Input)

	runRec := store.Run{
		TenantID:       in.TenantID,
		SessionID:      in.SessionID,
		Status:         core.RunQueued,
		Limits:         in.Limits.Defaults(),
		IdempotencyKey: in.IdempotencyKey,
		Workflow:       wfRaw,
		Input:          inRaw,
	}
	runID, err := o.Store.CreateRun(ctx, runRec)
	if err != nil {
		return "", err
	}
	o.Budget.Init(runID, in.Limits)

	runCtx, cancel := context.WithCancel(context.Background())
	if d := in.Limits.Defaults().MaxRuntime; d > 0 {
		runCtx, cancel = context.WithTimeout(runCtx, d)
	}

	st := &runState{cancel: cancel, done: make(chan struct{}), outputs: map[string]string{}}
	o.mu.Lock()
	o.runs[runID] = st
	o.mu.Unlock()

	go o.execute(runCtx, runID, in, st)
	return runID, nil
}

// Wait blocks until the run reaches a terminal state.
func (o *Orchestrator) Wait(ctx context.Context, runID core.RunID) (Result, error) {
	o.mu.Lock()
	st := o.runs[runID]
	o.mu.Unlock()
	if st == nil {
		return Result{}, core.ErrNotFound
	}
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-st.done:
		if st.err != nil {
			return *st.result, st.err
		}
		return *st.result, nil
	}
}

// Cancel signals the run to stop. It returns immediately; Wait observes the
// terminal status.
func (o *Orchestrator) Cancel(_ context.Context, runID core.RunID, _ string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	st, ok := o.runs[runID]
	if !ok {
		return core.ErrNotFound
	}
	st.cancel()
	return nil
}

// execute is the main run loop.
func (o *Orchestrator) execute(ctx context.Context, runID core.RunID, in StartRunInput, st *runState) {
	defer close(st.done)

	startedAt := time.Now().UTC()
	o.transitionRun(ctx, runID, in.TenantID, core.RunRunning, "")

	stageOutputs := map[string]string{}
	var finalErr error
	var finalStatus core.RunStatus = core.RunSucceeded
	var failureReason string

	for _, stage := range in.Workflow.Stages {
		if err := ctx.Err(); err != nil {
			finalErr = err
			finalStatus = core.RunCanceled
			failureReason = "context canceled"
			break
		}
		out, err := o.runStage(ctx, runID, in, stage, stageOutputs)
		if err != nil {
			finalErr = err
			if errors.Is(err, context.Canceled) {
				finalStatus = core.RunCanceled
				failureReason = "canceled"
			} else if errors.Is(err, context.DeadlineExceeded) {
				finalStatus = core.RunTimedOut
				failureReason = "deadline exceeded"
			} else {
				finalStatus = core.RunFailed
				failureReason = err.Error()
			}
			break
		}
		stageOutputs[stage.Name] = out
	}

	usage := o.Budget.Snapshot(runID)
	usage.Runtime = time.Since(startedAt)

	// Persist final state.
	if cur, err := o.Store.GetRun(context.Background(), in.TenantID, runID); err == nil {
		cur.Status = finalStatus
		cur.Usage = usage
		cur.FailureReason = failureReason
		cur.EndedAt = time.Now().UTC()
		out, _ := json.Marshal(stageOutputs)
		cur.Output = out
		_ = o.Store.UpdateRun(context.Background(), cur)
	}
	o.emit(context.Background(), core.Event{
		TenantID:   in.TenantID,
		SessionID:  in.SessionID,
		RunID:      runID,
		Type:       core.EvRunStateChanged,
		Visibility: core.VisibilityPublic,
		Payload:    mustJSON(map[string]any{"status": finalStatus, "failure_reason": failureReason}),
	})
	o.LoopDet.Reset(runID)

	st.result = &Result{
		RunID:         runID,
		Status:        finalStatus,
		Outputs:       stageOutputs,
		Usage:         usage,
		FailureReason: failureReason,
	}
	st.err = finalErr
}

// runStage runs one stage and returns its aggregated output.
func (o *Orchestrator) runStage(ctx context.Context, runID core.RunID, in StartRunInput, stage Stage, prior map[string]string) (string, error) {
	switch stage.Kind {
	case StageSequential, "":
		var last string
		for _, ag := range stage.Agents {
			out, err := o.runAgentStep(ctx, runID, in, stage.Name, ag, prior)
			if err != nil {
				return "", err
			}
			prior[stage.Name+"."+ag.Name] = out
			last = out
		}
		return last, nil
	case StageParallel:
		return o.runParallelStage(ctx, runID, in, stage, prior)
	default:
		return "", fmt.Errorf("unknown stage kind: %s", stage.Kind)
	}
}

func (o *Orchestrator) runParallelStage(ctx context.Context, runID core.RunID, in StartRunInput, stage Stage, prior map[string]string) (string, error) {
	if err := o.Budget.CheckSubAgent(runID); err != nil {
		return "", err
	}
	type result struct {
		idx int
		out string
		err error
	}
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(stage.Agents))
	priorCopy := map[string]string{}
	for k, v := range prior {
		priorCopy[k] = v
	}

	limits := in.Limits.Defaults()
	sem := make(chan struct{}, limits.MaxConcurrent)
	var wg sync.WaitGroup
	for i, ag := range stage.Agents {
		wg.Add(1)
		go func(idx int, ag AgentStep) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				results <- result{idx: idx, err: gctx.Err()}
				return
			}
			defer func() { <-sem }()
			out, err := o.runAgentStep(gctx, runID, in, stage.Name, ag, priorCopy)
			results <- result{idx: idx, out: out, err: err}
		}(i, ag)
	}
	wg.Wait()
	close(results)

	collected := make([]result, 0, len(stage.Agents))
	for r := range results {
		collected = append(collected, r)
	}
	sort.Slice(collected, func(i, j int) bool { return collected[i].idx < collected[j].idx })

	policy := stage.FailurePolicy
	if policy == "" {
		policy = FailFast
	}

	var firstErr error
	parts := make([]string, 0, len(collected))
	for _, r := range collected {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			if policy == FailFast {
				return "", r.err
			}
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", stage.Agents[r.idx].Name, r.out))
		prior[stage.Name+"."+stage.Agents[r.idx].Name] = r.out
	}

	switch policy {
	case WaitAll:
		if firstErr != nil {
			return "", firstErr
		}
	case BestEffort:
		// proceed even with partial failures, as long as at least one succeeded
		if len(parts) == 0 && firstErr != nil {
			return "", firstErr
		}
	}

	aggregated := joinParts(parts)
	return aggregated, nil
}

func joinParts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}

func (o *Orchestrator) runAgentStep(ctx context.Context, runID core.RunID, in StartRunInput, stageName string, step AgentStep, prior map[string]string) (string, error) {
	agent, ok := o.Agents[step.Name]
	if !ok {
		return "", fmt.Errorf("unknown agent: %s", step.Name)
	}
	if err := o.Budget.CheckSubAgent(runID); err != nil {
		return "", err
	}

	prompt := renderTemplate(step.Prompt, in.Input, prior)

	taskID, err := o.Store.CreateTask(ctx, store.Task{
		TenantID:  in.TenantID,
		RunID:     runID,
		AgentName: agent.Name,
		Status:    core.TaskRunning,
		Input:     mustJSON(map[string]any{"prompt": prompt, "stage": stageName}),
	})
	if err != nil {
		return "", err
	}
	o.emit(ctx, core.Event{
		TenantID:   in.TenantID,
		SessionID:  in.SessionID,
		RunID:      runID,
		TaskID:     taskID,
		Type:       core.EvTaskSpawned,
		Visibility: core.VisibilityInternal,
		Payload:    mustJSON(map[string]any{"agent": agent.Name, "stage": stageName, "prompt": prompt}),
	})
	_ = o.Budget.Commit(runID, core.Usage{SubAgents: 1})

	// Build initial messages.
	messages := []model.Message{}
	if agent.Instructions != "" {
		messages = append(messages, model.Message{Role: model.RoleSystem, Content: agent.Instructions})
	}
	messages = append(messages, model.Message{Role: model.RoleUser, Content: prompt})

	// Tool specs the model is allowed to call.
	toolSpecs := []model.ToolSpec{}
	allowed := map[string]tool.Definition{}
	for _, name := range agent.Tools {
		def, _, ok := o.Tools.Get(name)
		if !ok {
			continue
		}
		ok2, _, reason := o.Policy.Authorize(def)
		if !ok2 {
			o.emit(ctx, core.Event{
				TenantID:   in.TenantID,
				SessionID:  in.SessionID,
				RunID:      runID,
				TaskID:     taskID,
				Type:       core.EvSystem,
				Visibility: core.VisibilityInternal,
				Payload:    mustJSON(map[string]any{"event": "tool_denied", "tool": def.Name, "reason": reason}),
			})
			continue
		}
		allowed[def.Name] = def
		toolSpecs = append(toolSpecs, model.ToolSpec{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.InputSchema,
		})
	}

	maxLoops := agent.MaxToolLoops
	for loop := 0; loop < maxLoops; loop++ {
		if err := o.Budget.CheckModelCall(runID); err != nil {
			return "", o.failTask(ctx, in.TenantID, taskID, err)
		}
		stepID := core.StepID(core.NewID())
		o.emit(ctx, core.Event{
			TenantID:   in.TenantID,
			SessionID:  in.SessionID,
			RunID:      runID,
			TaskID:     taskID,
			StepID:     stepID,
			Type:       core.EvModelStarted,
			Visibility: core.VisibilityInternal,
			Payload:    mustJSON(map[string]any{"model": agent.Model, "agent": agent.Name}),
		})

		req := model.ChatRequest{
			TenantID: in.TenantID,
			RunID:    runID,
			StepID:   stepID,
			Model:    agent.Model,
			Messages: messages,
			Tools:    toolSpecs,
		}
		resp, err := agent.Provider.Chat(ctx, req)
		if err != nil {
			return "", o.failTask(ctx, in.TenantID, taskID, fmt.Errorf("model: %w", err))
		}
		_ = o.Budget.Commit(runID, resp.Usage)
		o.emit(ctx, core.Event{
			TenantID:      in.TenantID,
			SessionID:     in.SessionID,
			RunID:         runID,
			TaskID:        taskID,
			StepID:        stepID,
			Type:          core.EvModelCompleted,
			Visibility:    core.VisibilityInternal,
			Payload:       mustJSON(map[string]any{"usage": resp.Usage, "tool_calls": len(resp.ToolCalls)}),
			HiddenPayload: mustJSON(map[string]any{"content": resp.Message.Content}),
		})

		if len(resp.ToolCalls) == 0 {
			// Final answer.
			content := resp.Message.Content
			o.emit(ctx, core.Event{
				TenantID:   in.TenantID,
				SessionID:  in.SessionID,
				RunID:      runID,
				TaskID:     taskID,
				Type:       core.EvAgentMessage,
				Visibility: core.VisibilityPublic,
				Payload:    mustJSON(map[string]any{"agent": agent.Name, "content": content}),
			})
			_ = o.Store.AddMessage(ctx, store.MessageProjection{
				SessionID:  in.SessionID,
				RunID:      runID,
				Role:       "assistant:" + agent.Name,
				Visibility: core.VisibilityPublic,
				Content:    content,
			})
			t, _ := o.Store.GetRun(ctx, in.TenantID, runID)
			_ = t // silence unused if compiler complains
			task := store.Task{
				ID:        taskID,
				TenantID:  in.TenantID,
				RunID:     runID,
				AgentName: agent.Name,
				Status:    core.TaskCompleted,
				Output:    mustJSON(map[string]any{"content": content}),
			}
			_ = o.Store.UpdateTask(ctx, task)
			o.emit(ctx, core.Event{
				TenantID:   in.TenantID,
				SessionID:  in.SessionID,
				RunID:      runID,
				TaskID:     taskID,
				Type:       core.EvTaskCompleted,
				Visibility: core.VisibilityInternal,
				Payload:    mustJSON(map[string]any{"agent": agent.Name}),
			})
			return content, nil
		}

		// Execute tool calls (in parallel for parallel-safe tools).
		messages = append(messages, resp.Message)
		toolMessages, err := o.runToolCalls(ctx, in.TenantID, in.SessionID, runID, taskID, resp.ToolCalls, allowed)
		if err != nil {
			return "", o.failTask(ctx, in.TenantID, taskID, err)
		}
		messages = append(messages, toolMessages...)
	}

	return "", o.failTask(ctx, in.TenantID, taskID, fmt.Errorf("agent %s exceeded MaxToolLoops=%d", agent.Name, maxLoops))
}

func (o *Orchestrator) failTask(ctx context.Context, tenant core.TenantID, taskID core.TaskID, err error) error {
	_ = o.Store.UpdateTask(ctx, store.Task{
		ID:       taskID,
		TenantID: tenant,
		Status:   core.TaskFailed,
		Error:    err.Error(),
	})
	return err
}

func (o *Orchestrator) runToolCalls(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID, calls []model.ToolCall, allowed map[string]tool.Definition) ([]model.Message, error) {
	type toolOut struct {
		idx int
		msg model.Message
		err error
	}
	results := make(chan toolOut, len(calls))
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup

	for i, tc := range calls {
		def, ok := allowed[tc.Name]
		if !ok {
			results <- toolOut{idx: i, err: fmt.Errorf("tool not allowed: %s", tc.Name)}
			continue
		}
		if err := o.Budget.CheckToolCall(runID); err != nil {
			return nil, err
		}
		// Loop detection.
		if stalled, count := o.LoopDet.Observe(runID, tc.Name, hashInput(tc.Input)); stalled {
			o.emit(ctx, core.Event{
				TenantID:   tenant,
				SessionID:  session,
				RunID:      runID,
				TaskID:     taskID,
				Type:       core.EvSystem,
				Visibility: core.VisibilityAudit,
				Payload:    mustJSON(map[string]any{"event": "loop_detected", "tool": tc.Name, "count": count}),
			})
			return nil, fmt.Errorf("loop detected on tool %s after %d identical calls", tc.Name, count)
		}

		_, exec, _ := o.Tools.Get(tc.Name)
		callRec := tool.Call{
			TenantID: tenant,
			RunID:    runID,
			TaskID:   taskID,
			StepID:   core.StepID(core.NewID()),
			Tool:     def,
			Input:    tc.Input,
		}

		_ = o.Budget.Commit(runID, core.Usage{ToolCalls: 1})
		o.emit(ctx, core.Event{
			TenantID:   tenant,
			SessionID:  session,
			RunID:      runID,
			TaskID:     taskID,
			StepID:     callRec.StepID,
			Type:       core.EvToolScheduled,
			Visibility: core.VisibilityInternal,
			Payload:    mustJSON(map[string]any{"tool": def.Name, "input": json.RawMessage(tc.Input)}),
		})

		runParallel := def.Concurrency == tool.ConcurrencyParallelRead || def.Concurrency == tool.ConcurrencyParallelIsolatedWrite
		if runParallel {
			wg.Add(1)
			go func(idx int, tc model.ToolCall, rec tool.Call) {
				defer wg.Done()
				msg, err := o.invokeTool(gctx, exec, rec, tc, tenant, session, runID, taskID)
				results <- toolOut{idx: idx, msg: msg, err: err}
			}(i, tc, callRec)
		} else {
			msg, err := o.invokeTool(gctx, exec, callRec, tc, tenant, session, runID, taskID)
			results <- toolOut{idx: i, msg: msg, err: err}
		}
	}
	wg.Wait()
	close(results)

	collected := make([]toolOut, 0, len(calls))
	for r := range results {
		collected = append(collected, r)
	}
	sort.Slice(collected, func(i, j int) bool { return collected[i].idx < collected[j].idx })

	out := make([]model.Message, 0, len(collected))
	for _, r := range collected {
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, r.msg)
	}
	return out, nil
}

func (o *Orchestrator) invokeTool(ctx context.Context, exec tool.Executor, call tool.Call, tc model.ToolCall, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID) (model.Message, error) {
	o.emit(ctx, core.Event{
		TenantID:   tenant,
		SessionID:  session,
		RunID:      runID,
		TaskID:     taskID,
		StepID:     call.StepID,
		Type:       core.EvToolStarted,
		Visibility: core.VisibilityInternal,
		Payload:    mustJSON(map[string]any{"tool": call.Tool.Name}),
	})
	res, err := exec.Execute(ctx, call)
	if err != nil {
		o.emit(ctx, core.Event{
			TenantID:   tenant,
			SessionID:  session,
			RunID:      runID,
			TaskID:     taskID,
			StepID:     call.StepID,
			Type:       core.EvToolCompleted,
			Visibility: core.VisibilityInternal,
			Payload:    mustJSON(map[string]any{"tool": call.Tool.Name, "error": err.Error()}),
		})
		return model.Message{}, fmt.Errorf("tool %s: %w", call.Tool.Name, err)
	}
	o.emit(ctx, core.Event{
		TenantID:   tenant,
		SessionID:  session,
		RunID:      runID,
		TaskID:     taskID,
		StepID:     call.StepID,
		Type:       core.EvToolCompleted,
		Visibility: core.VisibilityInternal,
		Payload:    mustJSON(map[string]any{"tool": call.Tool.Name, "output": json.RawMessage(res.Output)}),
	})
	return model.Message{
		Role:       model.RoleTool,
		Name:       call.Tool.Name,
		ToolCallID: tc.ID,
		Content:    string(res.Output),
	}, nil
}

func (o *Orchestrator) transitionRun(ctx context.Context, runID core.RunID, tenant core.TenantID, status core.RunStatus, reason string) {
	r, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		return
	}
	r.Status = status
	if status == core.RunRunning && r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	r.FailureReason = reason
	_ = o.Store.UpdateRun(ctx, r)
	o.emit(ctx, core.Event{
		TenantID:   tenant,
		SessionID:  r.SessionID,
		RunID:      runID,
		Type:       core.EvRunStateChanged,
		Visibility: core.VisibilityPublic,
		Payload:    mustJSON(map[string]any{"status": status, "reason": reason}),
	})
}

func (o *Orchestrator) emit(ctx context.Context, ev core.Event) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	_, _ = o.Store.Append(ctx, []core.Event{ev})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func hashInput(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// renderTemplate substitutes {{input.<key>}} and {{stage.<name>}} placeholders.
// It is intentionally simple to keep dependencies minimal.
func renderTemplate(tmpl string, input map[string]string, prior map[string]string) string {
	out := tmpl
	for k, v := range input {
		out = replaceAll(out, "{{input."+k+"}}", v)
	}
	for k, v := range prior {
		out = replaceAll(out, "{{stage."+k+"}}", v)
	}
	return out
}

func replaceAll(s, old, new string) string {
	if old == "" {
		return s
	}
	out := ""
	for {
		idx := indexOf(s, old)
		if idx < 0 {
			out += s
			return out
		}
		out += s[:idx] + new
		s = s[idx+len(old):]
	}
}

func indexOf(s, sub string) int {
	if len(sub) == 0 || len(sub) > len(s) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
