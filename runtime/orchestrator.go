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
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
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

	history []model.Message
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
	// MaxLimits caps the limits a caller may request for a run.
	MaxLimits core.Limits
	// Log receives run lifecycle and error logs; NewOrchestrator sets slog.Default().
	Log *slog.Logger
	// ApprovalTimeout is how long a tool approval stays open; zero or less means it never expires.
	ApprovalTimeout time.Duration
	// MaxActiveRuns caps how many runs execute at once (zero means no cap); read at the first StartRun.
	MaxActiveRuns int
	// MaxQueuedRuns caps how many runs wait for a slot; read at the first StartRun.
	MaxQueuedRuns int
	// MaxHistoryMessages caps how many earlier session messages an agent sees; zero means all.
	MaxHistoryMessages int
	// IdleWarnAfter is how long a run may go without an event before it gets an idle_wait diagnostic; zero disables it.
	IdleWarnAfter time.Duration

	mu       sync.Mutex
	runs     map[core.RunID]*runState
	inflight int // admitted runs, from admit until queueAndExecute returns
	capacity int // MaxActiveRuns+MaxQueuedRuns when slots was made
	slots    chan struct{}
	queued   atomic.Int64
}

type runState struct {
	activity  sync.Mutex
	lastAt    time.Time
	lastEvent string

	tenant  core.TenantID
	cancel  context.CancelFunc
	done    chan struct{}
	result  *Result
	outputs map[string]string
}

type allowedTool struct {
	def              core.ToolDefinition
	requiresApproval bool
	approvalReason   string
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
		Store:              s,
		Tools:              reg,
		Agents:             am,
		Budget:             policy.NewBudgetManager(),
		LoopDet:            policy.NewLoopDetector(5),
		Approvals:          s,
		Log:                slog.Default(),
		ApprovalTimeout:    15 * time.Minute,
		IdleWarnAfter:      2 * time.Minute,
		MaxHistoryMessages: 50,
		MaxActiveRuns:      64,
		MaxQueuedRuns:      256,
		MaxLimits: core.Limits{
			MaxDepth:      8,
			MaxSubAgents:  64,
			MaxConcurrent: 16,
			MaxToolCalls:  256,
			MaxModelCalls: 256,
			MaxRuntime:    time.Hour,
			// Cost stays uncapped until a provider reports it.
			MaxPromptTokens:     4_000_000,
			MaxCompletionTokens: 1_000_000,
		},
		runs: map[core.RunID]*runState{},
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
	in.Limits = in.Limits.Defaults().Cap(o.MaxLimits)

	if in.IdempotencyKey != "" {
		if existing, err := o.Store.FindByIdempotency(ctx, in.TenantID, in.IdempotencyKey); err == nil {
			return existing.ID, nil
		}
	}

	if err := o.admit(); err != nil {
		return "", err
	}
	started := false
	defer func() {
		if !started {
			o.release()
		}
	}()

	wfRaw, _ := json.Marshal(in.Workflow)
	inRaw, _ := json.Marshal(in.Input)

	runRec := store.Run{
		TenantID:       in.TenantID,
		SessionID:      in.SessionID,
		Status:         core.RunQueued,
		Limits:         in.Limits,
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
	st := &runState{lastAt: time.Now(), tenant: in.TenantID, cancel: cancel, done: make(chan struct{}), outputs: map[string]string{}}
	o.mu.Lock()
	o.runs[runID] = st
	o.mu.Unlock()

	started = true
	go o.queueAndExecute(runCtx, runID, in, st)
	return runID, nil
}

// admit reserves room for one more executing or queued run, or returns
// core.ErrOverloaded. release gives the room back.
func (o *Orchestrator) admit() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.slots == nil && o.MaxActiveRuns > 0 {
		o.slots = make(chan struct{}, o.MaxActiveRuns)
		o.capacity = o.MaxActiveRuns + o.MaxQueuedRuns
	}
	if o.slots != nil && o.inflight >= o.capacity {
		return fmt.Errorf("%w: %d runs executing or queued", core.ErrOverloaded, o.capacity)
	}
	o.inflight++
	return nil
}

func (o *Orchestrator) release() {
	o.mu.Lock()
	o.inflight--
	o.mu.Unlock()
}

// queueAndExecute waits for a free run slot, then executes the run.
func (o *Orchestrator) queueAndExecute(ctx context.Context, runID core.RunID, in StartRunInput, st *runState) {
	defer o.release()
	if o.slots != nil {
		o.queued.Add(1)
		select {
		case o.slots <- struct{}{}:
			defer func() { <-o.slots }()
		case <-ctx.Done():
		}
		o.queued.Add(-1)
	}
	o.execute(ctx, runID, in, st)
}

// QueuedRuns returns how many runs are waiting for a slot.
func (o *Orchestrator) QueuedRuns() int { return int(o.queued.Load()) }

// Wait blocks until the tenant's run reaches a terminal state. A run this
// orchestrator is not executing is answered from the store.
func (o *Orchestrator) Wait(ctx context.Context, tenant core.TenantID, runID core.RunID) (Result, error) {
	o.mu.Lock()
	st := o.runs[runID]
	o.mu.Unlock()
	if st == nil || st.tenant != tenant {
		return o.storedResult(ctx, tenant, runID)
	}
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-st.done:
		return *st.result, runError(st.result.Status, st.result.FailureReason)
	}
}

func (o *Orchestrator) storedResult(ctx context.Context, tenant core.TenantID, runID core.RunID) (Result, error) {
	run, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		return Result{}, err
	}
	if !run.Status.Terminal() {
		return Result{}, fmt.Errorf("run %s is %s but not executing here: %w", runID, run.Status, core.ErrNotFound)
	}
	res := Result{RunID: runID, Status: run.Status, Usage: run.Usage, FailureReason: run.FailureReason}
	if len(run.Output) > 0 {
		if err := json.Unmarshal(run.Output, &res.Outputs); err != nil {
			return Result{}, fmt.Errorf("run %s output: %w", runID, err)
		}
	}
	return res, runError(run.Status, run.FailureReason)
}

func runError(status core.RunStatus, reason string) error {
	switch status {
	case core.RunSucceeded:
		return nil
	case core.RunCanceled:
		return fmt.Errorf("%s: %w", reason, context.Canceled)
	case core.RunTimedOut:
		return fmt.Errorf("%s: %w", reason, context.DeadlineExceeded)
	default:
		return errors.New(reason)
	}
}

// ActiveRuns returns how many runs this orchestrator is executing or has queued.
func (o *Orchestrator) ActiveRuns() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.runs)
}

// Cancel signals the tenant's run to stop. It returns immediately; Wait
// observes the terminal status. A run that already ended gives core.ErrConflict.
func (o *Orchestrator) Cancel(ctx context.Context, tenant core.TenantID, runID core.RunID, _ string) error {
	o.mu.Lock()
	st, ok := o.runs[runID]
	o.mu.Unlock()
	if ok && st.tenant == tenant {
		st.cancel()
		return nil
	}
	run, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return fmt.Errorf("run %s already %s: %w", runID, run.Status, core.ErrConflict)
	}
	return fmt.Errorf("run %s is not executing here: %w", runID, core.ErrNotFound)
}

// execute is the main run loop.
func (o *Orchestrator) execute(ctx context.Context, runID core.RunID, in StartRunInput, st *runState) {
	defer close(st.done)
	ctx, stopTimer := context.WithTimeout(ctx, in.Limits.MaxRuntime)
	defer stopTimer()

	startedAt := time.Now().UTC()
	log := o.Log.With("run_id", runID, "tenant_id", in.TenantID, "session_id", in.SessionID)
	log.InfoContext(ctx, "run started", "workflow", in.Workflow.Name)

	stageOutputs := map[string]string{}
	var finalStatus core.RunStatus = core.RunSucceeded
	var failureReason string

	stages := in.Workflow.Stages
	if ctx.Err() != nil {
		finalStatus = core.RunCanceled
		failureReason = "canceled while queued"
		stages = nil
	} else {
		o.transitionRun(ctx, runID, in.TenantID, core.RunRunning, "")
		history, err := o.sessionHistory(ctx, in.TenantID, in.SessionID, runID)
		if err != nil {
			finalStatus = core.RunFailed
			failureReason = "session history: " + err.Error()
			stages = nil
		}
		in.history = history
	}

	stopIdleWatch := func() {}
	if o.IdleWarnAfter > 0 {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { o.watchIdle(ctx, stop, log, runID, in, st) })
		stopIdleWatch = func() { close(stop); wg.Wait() }
	}

	for _, stage := range stages {
		if ctx.Err() != nil {
			finalStatus = core.RunCanceled
			failureReason = "context canceled"
			break
		}
		out, err := o.runStage(ctx, runID, in, stage, stageOutputs)
		if err != nil {
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

	stopIdleWatch()

	cur, err := o.Store.GetRun(context.Background(), in.TenantID, runID)
	if err == nil {
		cur.Status = finalStatus
		cur.Usage = usage
		cur.FailureReason = failureReason
		cur.EndedAt = time.Now().UTC()
		cur.Output, _ = json.Marshal(stageOutputs)
		err = o.Store.UpdateRun(context.Background(), cur)
	}
	o.logErr(ctx, "persist final run state", err, "run_id", runID)
	finalStored := err == nil
	o.emit(context.Background(), core.Event{
		TenantID:   in.TenantID,
		SessionID:  in.SessionID,
		RunID:      runID,
		Type:       core.EvRunStateChanged,
		Visibility: core.VisibilityPublic,
		Payload:    mustJSON(map[string]any{"status": finalStatus, "failure_reason": failureReason}),
	})
	o.LoopDet.Reset(runID)
	o.Budget.Forget(runID)
	// Wait answers released runs from the store, so keep the state when the final write failed.
	if finalStored {
		o.mu.Lock()
		delete(o.runs, runID)
		o.mu.Unlock()
	}
	level := slog.LevelInfo
	if finalStatus == core.RunFailed {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "run finished",
		"status", finalStatus,
		"failure_reason", failureReason,
		"duration", usage.Runtime,
		"prompt_tokens", usage.PromptTokens,
		"completion_tokens", usage.CompletionTokens,
		"model_calls", usage.ModelCalls,
		"tool_calls", usage.ToolCalls,
	)

	st.result = &Result{
		RunID:         runID,
		Status:        finalStatus,
		Outputs:       stageOutputs,
		Usage:         usage,
		FailureReason: failureReason,
	}
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
	for _, ag := range stage.Agents {
		if _, ok := o.Agents[ag.Name]; !ok {
			return "", fmt.Errorf("unknown agent: %s", ag.Name)
		}
	}
	if err := o.Budget.Reserve(runID, core.Usage{SubAgents: int64(len(stage.Agents))}); err != nil {
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

	sem := make(chan struct{}, in.Limits.MaxConcurrent)
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
			out, err := o.runAgentStepReserved(gctx, runID, in, stage.Name, ag, priorCopy, false)
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
	return o.runAgentStepReserved(ctx, runID, in, stageName, step, prior, true)
}

func (o *Orchestrator) runAgentStepReserved(ctx context.Context, runID core.RunID, in StartRunInput, stageName string, step AgentStep, prior map[string]string, reserveBudget bool) (string, error) {
	agent, ok := o.Agents[step.Name]
	if !ok {
		return "", fmt.Errorf("unknown agent: %s", step.Name)
	}
	reserved := false
	if reserveBudget {
		if err := o.Budget.ReserveSubAgent(runID); err != nil {
			return "", err
		}
		reserved = true
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
		if reserved {
			o.Budget.Release(runID, core.Usage{SubAgents: 1})
		}
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
	// Build initial messages.
	messages := []model.Message{}
	if agent.Instructions != "" {
		messages = append(messages, model.Message{Role: model.RoleSystem, Content: agent.Instructions})
	}
	messages = append(messages, in.history...)
	messages = append(messages, model.Message{Role: model.RoleUser, Content: prompt})

	// Tool specs the model is allowed to call.
	toolSpecs := []model.ToolSpec{}
	allowed := map[string]allowedTool{}
	for _, name := range agent.Tools {
		def, _, ok := o.Tools.Get(name)
		if !ok {
			continue
		}
		ok2, requiresApproval, reason := o.Policy.Authorize(def)
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
		allowed[def.Name] = allowedTool{def: def, requiresApproval: requiresApproval, approvalReason: reason}
		toolSpecs = append(toolSpecs, model.ToolSpec{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.InputSchema,
		})
	}

	maxLoops := agent.MaxToolLoops
	for loop := 0; loop < maxLoops; loop++ {
		if err := o.Budget.ReserveModelCall(runID); err != nil {
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
		usage := resp.Usage
		usage.ModelCalls = 0
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
		commitErr := o.Budget.Commit(runID, usage)
		o.persistUsage(ctx, in.TenantID, runID)
		if commitErr != nil {
			return "", o.failTask(ctx, in.TenantID, taskID, commitErr)
		}

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
			o.logErr(ctx, "add message", o.Store.AddMessage(ctx, store.MessageProjection{
				SessionID:  in.SessionID,
				RunID:      runID,
				Role:       "assistant:" + agent.Name,
				Visibility: core.VisibilityPublic,
				Content:    content,
			}), "run_id", runID)
			task := store.Task{
				ID:        taskID,
				TenantID:  in.TenantID,
				RunID:     runID,
				AgentName: agent.Name,
				Status:    core.TaskCompleted,
				Output:    mustJSON(map[string]any{"content": content}),
			}
			o.logErr(ctx, "update task", o.Store.UpdateTask(ctx, task), "run_id", runID, "task_id", taskID)
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

// sessionHistory returns the session's last MaxHistoryMessages messages from
// humans and from earlier runs; the current run's stage outputs reach agents
// through prompt templates instead. Only messages a run wrote are assistant
// turns, whatever role a client posted with.
func (o *Orchestrator) sessionHistory(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID) ([]model.Message, error) {
	stored, err := o.Store.ListMessages(ctx, tenant, session)
	if err != nil {
		return nil, err
	}
	var out []model.Message
	for _, m := range stored {
		if m.RunID == runID {
			continue
		}
		role := model.RoleUser
		if m.RunID != "" {
			role = model.RoleAssistant
		}
		out = append(out, model.Message{Role: role, Content: m.Content})
	}
	if o.MaxHistoryMessages > 0 && len(out) > o.MaxHistoryMessages {
		out = out[len(out)-o.MaxHistoryMessages:]
	}
	return out, nil
}

func (o *Orchestrator) failTask(ctx context.Context, tenant core.TenantID, taskID core.TaskID, err error) error {
	o.logErr(ctx, "update task", o.Store.UpdateTask(ctx, store.Task{
		ID:       taskID,
		TenantID: tenant,
		Status:   core.TaskFailed,
		Error:    err.Error(),
	}), "task_id", taskID)
	return err
}

func (o *Orchestrator) persistUsage(ctx context.Context, tenant core.TenantID, runID core.RunID) {
	o.logErr(ctx, "update run usage", o.Store.UpdateRunUsage(ctx, tenant, runID, o.Budget.Snapshot(runID)), "run_id", runID)
}

func (o *Orchestrator) logErr(ctx context.Context, op string, err error, attrs ...any) {
	if err != nil {
		o.Log.ErrorContext(ctx, op+" failed", append(attrs, "err", err)...)
	}
}

func (o *Orchestrator) runToolCalls(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID, calls []model.ToolCall, allowed map[string]allowedTool) ([]model.Message, error) {
	type toolOut struct {
		idx int
		msg model.Message
		err error
	}
	type plannedTool struct {
		idx              int
		tc               model.ToolCall
		call             tool.Call
		exec             tool.Executor
		requiresApproval bool
		approvalReason   string
		runParallel      bool
		refused          error
	}

	if err := o.Budget.ReserveToolCalls(runID, len(calls)); err != nil {
		return nil, err
	}
	reserved := core.Usage{ToolCalls: int64(len(calls))}
	plans := make([]plannedTool, 0, len(calls))
	preflightOK := false
	defer func() {
		if !preflightOK {
			o.Budget.Release(runID, reserved)
		}
	}()

	for i, tc := range calls {
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
		callRec := tool.Call{
			TenantID: tenant,
			RunID:    runID,
			TaskID:   taskID,
			StepID:   core.StepID(core.NewID()),
			Tool:     core.ToolDefinition{Name: tc.Name},
			Input:    tc.Input,
		}
		allowedTool, ok := allowed[tc.Name]
		if !ok {
			plans = append(plans, plannedTool{idx: i, tc: tc, call: callRec, refused: fmt.Errorf("%w: tool %s is not available to this agent", core.ErrPolicyDenied, tc.Name)})
			continue
		}
		_, exec, found := o.Tools.Get(tc.Name)
		if !found {
			plans = append(plans, plannedTool{idx: i, tc: tc, call: callRec, refused: fmt.Errorf("unknown tool %s", tc.Name)})
			continue
		}
		def := allowedTool.def
		callRec.Tool = def
		plans = append(plans, plannedTool{
			idx:              i,
			tc:               tc,
			call:             callRec,
			exec:             exec,
			requiresApproval: allowedTool.requiresApproval,
			approvalReason:   allowedTool.approvalReason,
			runParallel:      def.Concurrency == core.ConcurrencyParallelRead || def.Concurrency == core.ConcurrencyParallelIsolatedWrite,
		})
	}
	preflightOK = true
	o.persistUsage(ctx, tenant, runID)

	results := make(chan toolOut, len(calls))
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup

	for _, plan := range plans {
		o.emit(ctx, core.Event{
			TenantID:   tenant,
			SessionID:  session,
			RunID:      runID,
			TaskID:     taskID,
			StepID:     plan.call.StepID,
			Type:       core.EvToolScheduled,
			Visibility: core.VisibilityInternal,
			Payload:    mustJSON(map[string]any{"tool": plan.call.Tool.Name, "input": json.RawMessage(plan.tc.Input)}),
		})
	}

	for i := range plans {
		if plans[i].requiresApproval {
			status, err := o.awaitToolApproval(gctx, tenant, session, runID, taskID, plans[i].call, plans[i].approvalReason)
			if err != nil {
				return nil, err
			}
			if status != "approved" {
				plans[i].refused = fmt.Errorf("call to tool %s was not approved (%s)", plans[i].call.Tool.Name, status)
			}
		}
	}

	for _, plan := range plans {
		if plan.refused != nil {
			results <- toolOut{idx: plan.idx, msg: o.toolFailed(ctx, tenant, session, runID, taskID, plan.call, plan.tc, plan.refused)}
			continue
		}
		if plan.runParallel {
			wg.Add(1)
			go func(plan plannedTool) {
				defer wg.Done()
				msg, err := o.invokeTool(gctx, plan.exec, plan.call, plan.tc, tenant, session, runID, taskID)
				results <- toolOut{idx: plan.idx, msg: msg, err: err}
			}(plan)
		} else {
			wg.Wait()
			msg, err := o.invokeTool(gctx, plan.exec, plan.call, plan.tc, tenant, session, runID, taskID)
			results <- toolOut{idx: plan.idx, msg: msg, err: err}
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

func (o *Orchestrator) awaitToolApproval(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID, call tool.Call, reason string) (string, error) {
	if reason == "" {
		reason = "tool requires approval"
	}
	var expiresAt time.Time
	if o.ApprovalTimeout > 0 {
		expiresAt = time.Now().UTC().Add(o.ApprovalTimeout)
	}
	approvalID, err := o.Approvals.CreateApproval(ctx, store.Approval{
		TenantID:  tenant,
		RunID:     runID,
		StepID:    call.StepID,
		Reason:    reason,
		ExpiresAt: expiresAt,
		Request: mustJSON(map[string]any{
			"tool":  call.Tool.Name,
			"input": json.RawMessage(call.Input),
		}),
	})
	if err != nil {
		return "", err
	}
	o.transitionRun(ctx, runID, tenant, core.RunAwaitingApproval, reason)
	o.emit(ctx, core.Event{
		TenantID:   tenant,
		SessionID:  session,
		RunID:      runID,
		TaskID:     taskID,
		StepID:     call.StepID,
		Type:       core.EvApprovalReq,
		Visibility: core.VisibilityParticipant,
		Payload: mustJSON(map[string]any{
			"approval_id": approvalID,
			"tool":        call.Tool.Name,
			"reason":      reason,
			"expires_at":  expiresAt,
		}),
	})

	waitCtx, cancel := context.WithCancel(ctx)
	if !expiresAt.IsZero() {
		waitCtx, cancel = context.WithDeadline(ctx, expiresAt)
	}
	approval, waitErr := o.Approvals.WaitForApproval(waitCtx, approvalID)
	cancel()
	if waitErr != nil {
		status := "expired"
		if ctx.Err() != nil {
			status = "canceled"
		} else if !errors.Is(waitErr, context.DeadlineExceeded) {
			return "", fmt.Errorf("approval %s: %w", approvalID, waitErr)
		}
		approval, err = o.closeApproval(context.WithoutCancel(ctx), tenant, approvalID, status)
		if err != nil {
			return "", fmt.Errorf("approval %s: %w", approvalID, err)
		}
	}
	o.emit(context.WithoutCancel(ctx), core.Event{
		TenantID:   tenant,
		SessionID:  session,
		RunID:      runID,
		TaskID:     taskID,
		StepID:     call.StepID,
		Type:       core.EvApprovalRes,
		Visibility: core.VisibilityParticipant,
		Payload: mustJSON(map[string]any{
			"approval_id": approvalID,
			"tool":        call.Tool.Name,
			"status":      approval.Status,
		}),
	})
	if ctx.Err() != nil {
		return "", fmt.Errorf("approval %s: %w", approvalID, ctx.Err())
	}
	o.transitionRun(ctx, runID, tenant, core.RunRunning, "")
	return approval.Status, nil
}

// closeApproval resolves an open approval with status, or returns the
// decision a human recorded first.
func (o *Orchestrator) closeApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID, status string) (store.Approval, error) {
	if err := o.Approvals.ResolveApproval(ctx, tenant, id, status, nil); err != nil && !errors.Is(err, core.ErrConflict) {
		return store.Approval{}, err
	}
	return o.Approvals.GetApproval(ctx, tenant, id)
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
	res, err := safeExecute(ctx, exec, call)
	if err != nil {
		if ctx.Err() != nil {
			o.toolFailed(ctx, tenant, session, runID, taskID, call, tc, err)
			return model.Message{}, fmt.Errorf("tool %s: %w", call.Tool.Name, err)
		}
		return o.toolFailed(ctx, tenant, session, runID, taskID, call, tc, err), nil
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

func safeExecute(ctx context.Context, exec tool.Executor, call tool.Call) (res tool.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return exec.Execute(ctx, call)
}

func (o *Orchestrator) toolFailed(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID, call tool.Call, tc model.ToolCall, err error) model.Message {
	o.Log.WarnContext(ctx, "tool call failed", "run_id", runID, "task_id", taskID, "tool", call.Tool.Name, "err", err)
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
	return model.Message{
		Role:       model.RoleTool,
		Name:       call.Tool.Name,
		ToolCallID: tc.ID,
		Content:    string(mustJSON(map[string]string{"error": err.Error()})),
	}
}

func (o *Orchestrator) transitionRun(ctx context.Context, runID core.RunID, tenant core.TenantID, status core.RunStatus, reason string) {
	r, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		o.logErr(ctx, "read run", err, "run_id", runID)
		return
	}
	o.logErr(ctx, "update run status", o.Store.UpdateRunStatus(ctx, tenant, runID, status, reason), "run_id", runID)
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
	o.appendEvent(ctx, ev)
	o.mu.Lock()
	st := o.runs[ev.RunID]
	o.mu.Unlock()
	if st != nil {
		st.activity.Lock()
		st.lastAt, st.lastEvent = time.Now(), ev.Type
		st.activity.Unlock()
	}
}

func (o *Orchestrator) appendEvent(ctx context.Context, ev core.Event) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	_, err := o.Store.Append(ctx, []core.Event{ev})
	o.logErr(ctx, "append event", err, "run_id", ev.RunID, "type", ev.Type)
}

// watchIdle emits an idle_wait diagnostic, without counting it as activity,
// whenever the run has emitted nothing for IdleWarnAfter.
func (o *Orchestrator) watchIdle(ctx context.Context, stop <-chan struct{}, log *slog.Logger, runID core.RunID, in StartRunInput, st *runState) {
	timer := time.NewTimer(o.IdleWarnAfter)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-timer.C:
		}
		st.activity.Lock()
		idle, last := time.Since(st.lastAt), st.lastEvent
		st.activity.Unlock()
		if idle < o.IdleWarnAfter {
			timer.Reset(o.IdleWarnAfter - idle)
			continue
		}
		log.WarnContext(ctx, "run idle", "idle_for", idle, "last_event", last)
		o.appendEvent(ctx, core.Event{
			TenantID:   in.TenantID,
			SessionID:  in.SessionID,
			RunID:      runID,
			Type:       core.EvSystem,
			Visibility: core.VisibilityInternal,
			Payload:    mustJSON(map[string]any{"event": "idle_wait", "idle_for": idle.String(), "last_event": last}),
		})
		timer.Reset(o.IdleWarnAfter)
	}
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
