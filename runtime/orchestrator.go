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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/observe"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

// StartRunInput carries everything the orchestrator needs to begin.
type StartRunInput struct {
	TenantID       core.TenantID
	PrincipalID    core.PrincipalID
	SessionID      core.SessionID
	IdempotencyKey string
	Workflow       Workflow
	// Template names one of Orchestrator.Workflows to run instead of Workflow.
	Template string
	Input    map[string]string // available as {{input.<key>}} in templates
	// Images are named input images that agent steps show the model.
	Images   map[string]model.Image
	Limits   core.Limits
	Metadata map[string]any

	history []model.Message
	parent  core.RunID
	reuse   *completedTasks
	plan    *plan
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
	Store  store.Store // any driver: store.MemStore or store/pg.Store
	Tools  *tool.Registry
	Agents map[string]Agent
	// Workflows are the named workflow templates a run can start by name.
	Workflows map[string]Workflow
	Policy    policy.ToolPolicy
	Budget    *policy.BudgetManager
	LoopDet   *policy.LoopDetector
	Approvals store.ApprovalStore // typically the same Store
	// MaxLimits caps the limits a caller may request for a run.
	MaxLimits core.Limits
	// Log receives run lifecycle and error logs; NewOrchestrator sets slog.Default().
	Log *slog.Logger
	// Tracer records a span per run, stage, task, model call, tool call and approval wait;
	// NewOrchestrator uses the global OpenTelemetry tracer provider.
	Tracer trace.Tracer
	// Metrics records run, model and tool metrics; nil disables them. NewOrchestrator registers
	// the queue gauges on the instance it creates.
	Metrics *observe.Metrics
	// ApprovalTimeout is how long a tool approval stays open; zero or less means it never expires.
	ApprovalTimeout time.Duration
	// MaxActiveRuns caps how many runs execute at once (zero means no cap); read at the first StartRun.
	MaxActiveRuns int
	// MaxQueuedRuns caps how many runs wait for a slot; read at the first StartRun.
	MaxQueuedRuns int
	// Concurrency bounds runs per tenant and workflow, model calls per agent, and executions per
	// tool; set it before the first StartRun. A run waiting for its tenant or workflow slot counts
	// toward MaxQueuedRuns.
	Concurrency policy.ConcurrencyLimits
	// MaxHistoryMessages caps how many earlier session messages an agent sees; zero means all.
	MaxHistoryMessages int
	// Compaction summarizes long session history when a run starts.
	Compaction Compaction
	// IdleWarnAfter is how long a run may go without an event before it gets an idle_wait diagnostic; zero disables it.
	IdleWarnAfter time.Duration
	// LoopResponse is what a detected runaway loop does to its run.
	LoopResponse LoopResponse
	// ApproveAbove asks a human, once per execution, before a model call of a run whose total tokens or cost units passed its positive fields.
	ApproveAbove core.Usage
	// MaxArtifactBytes caps the content of each stored artifact, input images included; zero means no cap.
	MaxArtifactBytes int64

	mu        sync.Mutex
	runs      map[core.RunID]*runState
	inflight  int            // admitted runs, from admit until queueAndExecute returns
	admitted  sync.WaitGroup // counts the same runs, for Shutdown
	closing   bool
	base      context.Context // parent of every run context; Shutdown cancels it
	stopRuns  context.CancelCauseFunc
	capacity  int // MaxActiveRuns+MaxQueuedRuns when slots was made
	slots     chan struct{}
	queued    atomic.Int64
	executing atomic.Int64
	sems      policy.Semaphores
	deltas    deltaHub
	// skipApprovals runs tool calls and passes the cost threshold without
	// approval, for re-execution, whose recorded results already reflect each decision.
	skipApprovals bool
}

type runState struct {
	activity    sync.Mutex
	lastAt      time.Time
	lastEvent   string
	taskSpawned map[core.TaskID]core.EventID
	lastInStep  map[core.StepID]core.EventID
	taskActor   map[core.TaskID]core.PrincipalID

	cost costGate

	tenant  core.TenantID
	cancel  context.CancelCauseFunc
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
	o := &Orchestrator{
		Metrics:            observe.New(),
		Tracer:             otel.Tracer("github.com/nvcnvn/agent-loop/runtime"),
		Store:              s,
		Tools:              reg,
		Agents:             am,
		Workflows:          map[string]Workflow{},
		Budget:             policy.NewBudgetManager(),
		LoopDet:            policy.NewLoopDetector(5),
		Approvals:          s,
		Log:                slog.Default(),
		ApprovalTimeout:    15 * time.Minute,
		IdleWarnAfter:      2 * time.Minute,
		MaxArtifactBytes:   10 << 20,
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
	o.base, o.stopRuns = context.WithCancelCause(context.Background())
	o.Metrics.Gauge("agentloop_runs_queued", func() float64 { return float64(o.QueuedRuns()) })
	o.Metrics.Gauge("agentloop_runs_executing", func() float64 { return float64(o.executing.Load()) })
	return o
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
			if existing.SessionID != in.SessionID {
				return "", fmt.Errorf("idempotency key used by a run of another session: %w", core.ErrConflict)
			}
			return existing.ID, nil
		}
	}
	if in.Template != "" {
		if w := in.Workflow; len(w.Stages) > 0 || w.ToolPolicy != nil || w.Name != "" || len(w.InputSchema) > 0 {
			return "", errors.New("give a workflow or a template, not both")
		}
		wf, ok := o.Workflows[in.Template]
		if !ok {
			return "", fmt.Errorf("workflow template %q: %w", in.Template, core.ErrNotFound)
		}
		in.Workflow = wf
	}
	p, err := compileWorkflow(in.Workflow, o.Agents)
	if err != nil {
		return "", err
	}
	if err := p.checkInput(in.Input); err != nil {
		return "", err
	}
	for _, stage := range in.Workflow.Stages {
		for _, step := range stage.Agents {
			for _, name := range step.Images {
				if _, ok := in.Images[name]; !ok {
					return "", fmt.Errorf("stage %q: agent %s shows image %q, which the run input lacks", stage.Name, step.Name, name)
				}
			}
		}
	}
	for name, img := range in.Images {
		if err := o.checkArtifactSize("image "+name, len(img.Data)); err != nil {
			return "", err
		}
	}
	in.plan = p

	if err := o.admit(false); err != nil {
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
		ParentRunID:    in.parent,
		StartedBy:      in.PrincipalID,
		Workflow:       wfRaw,
		Input:          inRaw,
	}
	runID, err := o.Store.CreateRun(ctx, runRec)
	if err != nil {
		return "", err
	}
	for name, img := range in.Images {
		if _, err := o.Store.PutArtifact(ctx, store.Artifact{
			TenantID: in.TenantID, SessionID: in.SessionID, RunID: runID,
			Kind: inputImageKind, Name: name, MimeType: img.MimeType, Content: img.Data,
		}); err != nil {
			return "", err
		}
	}
	o.Budget.Init(runID, in.Limits)
	if err := o.launch(runID, in); err != nil {
		return "", err
	}
	started = true
	return runID, nil
}

// launch starts the goroutine that queues and executes an admitted run.
func (o *Orchestrator) launch(runID core.RunID, in StartRunInput) error {
	runCtx, st, err := o.claim(runID, in.TenantID)
	if err != nil {
		return err
	}
	o.emit(runCtx, core.Event{
		TenantID:   in.TenantID,
		SessionID:  in.SessionID,
		RunID:      runID,
		Type:       core.EvRunStateChanged,
		Visibility: core.VisibilityPublic,
		ActorID:    in.PrincipalID,
		Payload:    mustJSON(map[string]any{"status": core.RunQueued, "reason": "started"}),
	})
	go o.queueAndExecute(runCtx, runID, in, st)
	return nil
}

// claim registers the run as executing in this process, or returns
// core.ErrConflict if it already is.
func (o *Orchestrator) claim(runID core.RunID, tenant core.TenantID) (context.Context, *runState, error) {
	parent := o.base
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithCancelCause(parent)
	st := &runState{
		lastAt: time.Now(), tenant: tenant, cancel: cancel, done: make(chan struct{}), outputs: map[string]string{},
		taskSpawned: map[core.TaskID]core.EventID{}, lastInStep: map[core.StepID]core.EventID{}, taskActor: map[core.TaskID]core.PrincipalID{},
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, running := o.runs[runID]; running {
		cancel(nil)
		return nil, nil, fmt.Errorf("run %s is already executing: %w", runID, core.ErrConflict)
	}
	o.runs[runID] = st
	return runCtx, st, nil
}

// unclaim undoes a claim whose run never started.
func (o *Orchestrator) unclaim(runID core.RunID, st *runState) {
	st.cancel(nil)
	o.mu.Lock()
	delete(o.runs, runID)
	o.mu.Unlock()
	close(st.done)
}

// Resume continues the tenant's run that is not executing in this process and
// is unfinished, for example after a restart, or failed or timed out. Agent
// tasks that completed are reused; a task that was interrupted or failed runs
// again from its start. Succeeded and canceled runs give core.ErrConflict. by
// is who asked, the actor of the events that record it.
func (o *Orchestrator) Resume(ctx context.Context, tenant core.TenantID, runID core.RunID, by core.PrincipalID) error {
	return o.resume(ctx, tenant, runID, by, false)
}

// Recover resumes every unfinished run in the store and returns how many it
// resumed. Recovered runs bypass MaxQueuedRuns so none is left behind. It
// assumes this process is the only one executing runs.
func (o *Orchestrator) Recover(ctx context.Context) (int, error) {
	runs, err := o.Store.ListUnfinishedRuns(ctx)
	if err != nil {
		return 0, err
	}
	var errs []error
	resumed := 0
	for _, run := range runs {
		if run.Status == core.RunPaused {
			continue
		}
		if err := o.resume(ctx, run.TenantID, run.ID, core.SystemPrincipal, true); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", run.ID, err))
			continue
		}
		resumed++
	}
	return resumed, errors.Join(errs...)
}

func (o *Orchestrator) resume(ctx context.Context, tenant core.TenantID, runID core.RunID, by core.PrincipalID, force bool) error {
	runCtx, st, err := o.claim(runID, tenant)
	if err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			o.unclaim(runID, st)
		}
	}()
	// Read only after claiming, so a run that just finished here is seen as terminal.
	run, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		return err
	}
	if run.Status == core.RunSucceeded || run.Status == core.RunCanceled {
		return fmt.Errorf("run %s already %s: %w", run.ID, run.Status, core.ErrConflict)
	}
	in := StartRunInput{TenantID: run.TenantID, SessionID: run.SessionID, Limits: run.Limits}
	if err := json.Unmarshal(run.Workflow, &in.Workflow); err != nil {
		return fmt.Errorf("stored workflow: %w", err)
	}
	if len(run.Input) > 0 {
		if err := json.Unmarshal(run.Input, &in.Input); err != nil {
			return fmt.Errorf("stored input: %w", err)
		}
	}
	if in.plan, err = compileWorkflow(in.Workflow, o.Agents); err != nil {
		return fmt.Errorf("stored workflow: %w", err)
	}
	artifacts, err := o.Store.ListRunArtifacts(ctx, run.TenantID, run.ID)
	if err != nil {
		return err
	}
	for _, a := range artifacts {
		if a.Kind == inputImageKind && a.Producer == "" {
			if in.Images == nil {
				in.Images = map[string]model.Image{}
			}
			in.Images[a.Name] = model.Image{MimeType: a.MimeType, Data: a.Content}
		}
	}
	reuse, attempts, err := o.reconcileTasks(ctx, run)
	if err != nil {
		return err
	}
	in.reuse = reuse
	if err := o.Approvals.CancelRunApprovals(ctx, run.TenantID, run.ID); err != nil {
		return err
	}
	if err := o.admit(force); err != nil {
		return err
	}
	if run.Status.Terminal() || run.Status == core.RunPaused {
		reopened := run
		reopened.Status, reopened.FailureReason, reopened.EndedAt = core.RunQueued, "", time.Time{}
		if err := o.Store.UpdateRun(ctx, reopened); err != nil {
			o.release()
			return err
		}
		o.emit(ctx, core.Event{
			TenantID:   run.TenantID,
			SessionID:  run.SessionID,
			RunID:      run.ID,
			Type:       core.EvRunStateChanged,
			Visibility: core.VisibilityPublic,
			ActorID:    by,
			Payload:    mustJSON(map[string]any{"status": core.RunQueued, "reason": "resumed"}),
		})
	}
	usage := run.Usage
	// A parallel stage reserves all its agents up front, so the stored count
	// can include agents that never started; tasks are what actually ran.
	usage.SubAgents = int64(attempts)
	o.Budget.Init(run.ID, run.Limits)
	_ = o.Budget.Commit(run.ID, usage) // over budget already: the next reservation fails the run
	o.emit(ctx, core.Event{
		TenantID:   run.TenantID,
		SessionID:  run.SessionID,
		RunID:      run.ID,
		Type:       core.EvSystem,
		Visibility: core.VisibilityInternal,
		ActorID:    by,
		Payload:    mustJSON(map[string]any{"event": "run_resumed", "from_status": run.Status, "reused_tasks": reuse.count()}),
	})
	started = true
	go o.queueAndExecute(runCtx, run.ID, in, st)
	return nil
}

// reconcileTasks collects the outputs of the run's completed tasks, marks
// tasks that were still running as failed, and returns how many tasks the
// run has created.
func (o *Orchestrator) reconcileTasks(ctx context.Context, run store.Run) (*completedTasks, int, error) {
	tasks, err := o.Store.ListRunTasks(ctx, run.TenantID, run.ID)
	if err != nil {
		return nil, 0, err
	}
	c := &completedTasks{outputs: map[string][]string{}}
	for _, t := range tasks {
		switch t.Status {
		case core.TaskCompleted:
			var in struct{ Prompt, Stage string }
			var out struct{ Content string }
			if json.Unmarshal(t.Input, &in) != nil || json.Unmarshal(t.Output, &out) != nil {
				continue
			}
			key := taskKey(in.Stage, t.AgentName, in.Prompt)
			c.outputs[key] = append(c.outputs[key], out.Content)
		case core.TaskPending, core.TaskRunning:
			t.Status, t.Error = core.TaskFailed, "interrupted"
			if err := o.Store.UpdateTask(ctx, t); err != nil {
				return nil, 0, err
			}
		}
	}
	return c, len(tasks), nil
}

// completedTasks holds reusable task outputs keyed by stage, agent and prompt.
type completedTasks struct {
	mu      sync.Mutex
	outputs map[string][]string
}

func taskKey(stage, agent, prompt string) string { return stage + "\x00" + agent + "\x00" + prompt }

func (c *completedTasks) take(stage, agent, prompt string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := taskKey(stage, agent, prompt)
	outs := c.outputs[key]
	if len(outs) == 0 {
		return "", false
	}
	c.outputs[key] = outs[1:]
	return outs[0], true
}

// toRun returns how many of the stage's agent steps have no reusable output.
func (c *completedTasks) toRun(stage string, steps []AgentStep, prompts []string) int {
	if c == nil {
		return len(steps)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	used := map[string]int{}
	n := 0
	for i, step := range steps {
		key := taskKey(stage, step.Name, prompts[i])
		if used[key] < len(c.outputs[key]) {
			used[key]++
			continue
		}
		n++
	}
	return n
}

func (c *completedTasks) count() int {
	n := 0
	for _, outs := range c.outputs {
		n += len(outs)
	}
	return n
}

// admit reserves room for one more executing or queued run, or returns
// core.ErrOverloaded when the queue is full (unless force) or the
// orchestrator is shutting down. release gives the room back.
func (o *Orchestrator) admit(force bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closing {
		return fmt.Errorf("%w: shutting down", core.ErrOverloaded)
	}
	if o.slots == nil && o.MaxActiveRuns > 0 {
		o.slots = make(chan struct{}, o.MaxActiveRuns)
		o.capacity = o.MaxActiveRuns + o.MaxQueuedRuns
	}
	if !force && o.slots != nil && o.inflight >= o.capacity {
		return fmt.Errorf("%w: %d runs executing or queued", core.ErrOverloaded, o.capacity)
	}
	o.inflight++
	o.admitted.Add(1)
	return nil
}

func (o *Orchestrator) release() {
	o.mu.Lock()
	o.inflight--
	o.mu.Unlock()
	o.admitted.Done()
}

// errShutdown is the cancellation cause of runs that Shutdown checkpoints.
var errShutdown = errors.New("orchestrator shutting down")

// Shutdown stops admitting runs and waits for admitted ones to finish. When
// ctx ends first, it stops the remaining runs without recording a terminal
// status, so Recover resumes them in the next process, and returns ctx.Err().
// Wait on such a run returns core.ErrNotFound. It needs an orchestrator made
// by NewOrchestrator.
func (o *Orchestrator) Shutdown(ctx context.Context) error {
	o.mu.Lock()
	o.closing = true
	o.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		o.admitted.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
	}
	o.stopRuns(errShutdown)
	<-drained
	return ctx.Err()
}

// queueAndExecute waits for the run's tenant, workflow and global slots, then
// executes the run.
func (o *Orchestrator) queueAndExecute(ctx context.Context, runID core.RunID, in StartRunInput, st *runState) {
	defer o.release()
	o.queued.Add(1)
	releaseSlots, err := o.acquireRunSlots(ctx, in)
	o.queued.Add(-1)
	if err == nil {
		defer releaseSlots()
	}
	o.executing.Add(1)
	defer o.executing.Add(-1)
	o.execute(ctx, runID, in, st)
}

// acquireRunSlots takes the tenant and workflow slots before the global one,
// so a run blocked on its own tenant or workflow does not hold a global slot.
func (o *Orchestrator) acquireRunSlots(ctx context.Context, in StartRunInput) (func(), error) {
	var releases []func()
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	keys := []struct {
		key   string
		limit int
	}{{"tenant:" + string(in.TenantID), o.Concurrency.PerTenant}}
	if in.Workflow.Name != "" {
		keys = append(keys, struct {
			key   string
			limit int
		}{"workflow:" + string(in.TenantID) + "/" + in.Workflow.Name, o.Concurrency.PerWorkflow})
	}
	for _, k := range keys {
		release, err := o.sems.Acquire(ctx, k.key, k.limit)
		if err != nil {
			releaseAll()
			return nil, err
		}
		releases = append(releases, release)
	}
	if o.slots != nil {
		select {
		case o.slots <- struct{}{}:
			releases = append(releases, func() { <-o.slots })
		case <-ctx.Done():
			releaseAll()
			return nil, ctx.Err()
		}
	}
	return releaseAll, nil
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
		if st.result == nil {
			return o.storedResult(ctx, tenant, runID)
		}
		return *st.result, runError(st.result.Status, st.result.FailureReason)
	}
}

func (o *Orchestrator) storedResult(ctx context.Context, tenant core.TenantID, runID core.RunID) (Result, error) {
	run, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		return Result{}, err
	}
	if !run.Status.Terminal() && run.Status != core.RunPaused {
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

// Cancel signals the tenant's run to stop, recording who asked and why in an
// audit event. It returns immediately; Wait observes the terminal status. A
// run that already ended gives core.ErrConflict.
func (o *Orchestrator) Cancel(ctx context.Context, tenant core.TenantID, runID core.RunID, by core.PrincipalID, reason string) error {
	o.mu.Lock()
	st, ok := o.runs[runID]
	o.mu.Unlock()
	if ok && st.tenant == tenant {
		run, err := o.Store.GetRun(ctx, tenant, runID)
		if err != nil {
			return err
		}
		if run.Status.Terminal() {
			return fmt.Errorf("run %s already %s: %w", runID, run.Status, core.ErrConflict)
		}
		o.emit(ctx, core.Event{
			TenantID:   tenant,
			SessionID:  run.SessionID,
			RunID:      runID,
			Type:       core.EvSystem,
			Visibility: core.VisibilityAudit,
			ActorID:    by,
			Payload:    mustJSON(map[string]any{"event": "run_cancel_requested", "reason": reason}),
		})
		st.cancel(nil)
		return nil
	}
	run, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return fmt.Errorf("run %s already %s: %w", runID, run.Status, core.ErrConflict)
	}
	if run.Status != core.RunPaused {
		return fmt.Errorf("run %s is not executing here: %w", runID, core.ErrNotFound)
	}
	return o.cancelPaused(ctx, tenant, runID, by, reason)
}

// cancelPaused cancels a paused run, holding its claim so it cannot be resumed meanwhile.
func (o *Orchestrator) cancelPaused(ctx context.Context, tenant core.TenantID, runID core.RunID, by core.PrincipalID, reason string) error {
	_, st, err := o.claim(runID, tenant)
	if err != nil {
		return err
	}
	defer o.unclaim(runID, st)
	run, err := o.Store.GetRun(ctx, tenant, runID)
	if err != nil {
		return err
	}
	if run.Status != core.RunPaused {
		return fmt.Errorf("run %s is %s, no longer paused: %w", runID, run.Status, core.ErrConflict)
	}
	o.emit(ctx, core.Event{
		TenantID: tenant, SessionID: run.SessionID, RunID: runID, Type: core.EvSystem, Visibility: core.VisibilityAudit, ActorID: by,
		Payload: mustJSON(map[string]any{"event": "run_cancel_requested", "reason": reason}),
	})
	run.Status, run.FailureReason, run.EndedAt = core.RunCanceled, "canceled while paused", time.Now().UTC()
	if err := o.Store.UpdateRun(ctx, run); err != nil {
		return err
	}
	o.emit(ctx, core.Event{
		TenantID: tenant, SessionID: run.SessionID, RunID: runID, Type: core.EvRunStateChanged, Visibility: core.VisibilityPublic, ActorID: by,
		Payload: mustJSON(map[string]any{"status": core.RunCanceled, "failure_reason": run.FailureReason}),
	})
	return nil
}

// execute is the main run loop.
func (o *Orchestrator) execute(ctx context.Context, runID core.RunID, in StartRunInput, st *runState) {
	defer close(st.done)
	ctx, stopTimer := context.WithTimeout(ctx, in.Limits.MaxRuntime)
	defer stopTimer()

	startedAt := time.Now().UTC()
	stageOutputs := map[string]string{}
	var finalStatus core.RunStatus = core.RunSucceeded
	var failureReason string
	var runErr error

	ctx, span := o.startSpan(ctx, "run "+in.Workflow.Name,
		attribute.String("agentloop.run_id", string(runID)),
		attribute.String("agentloop.tenant_id", string(in.TenantID)),
		attribute.String("agentloop.session_id", string(in.SessionID)))
	defer func() {
		span.SetAttributes(attribute.String("agentloop.run_status", string(finalStatus)))
		var err error
		if finalStatus != core.RunSucceeded && failureReason != "" {
			err = errors.New(failureReason)
		}
		endSpan(span, err)
	}()
	log := o.Log.With("run_id", runID, "tenant_id", in.TenantID, "session_id", in.SessionID)
	if span.SpanContext().IsValid() {
		log = log.With("trace_id", span.SpanContext().TraceID().String())
	}
	log.InfoContext(ctx, "run started", "workflow", in.Workflow.Name)

	runnable := false
	if ctx.Err() != nil {
		finalStatus = core.RunCanceled
		failureReason = "canceled while queued"
	} else {
		o.transitionRun(ctx, runID, in.TenantID, core.RunRunning, "")
		history, err := o.sessionHistory(ctx, in, runID)
		if err != nil {
			finalStatus = core.RunFailed
			failureReason = "session history: " + err.Error()
			runErr = fmt.Errorf("session history: %w", err)
		}
		in.history = history
		runnable = err == nil
	}

	stopIdleWatch := func() {}
	if o.IdleWarnAfter > 0 {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { o.watchIdle(ctx, stop, log, runID, in, st) })
		stopIdleWatch = func() { close(stop); wg.Wait() }
	}

	if runnable {
		err := o.runStages(ctx, runID, in, in.plan, stageOutputs)
		runErr = err
		switch {
		case err == nil:
		case errors.Is(err, errPaused):
			finalStatus = core.RunPaused
			failureReason = err.Error()
		case errors.Is(err, context.Canceled):
			finalStatus = core.RunCanceled
			failureReason = "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			finalStatus = core.RunTimedOut
			failureReason = "deadline exceeded"
			if ctx.Err() == nil {
				failureReason = err.Error()
			}
		default:
			finalStatus = core.RunFailed
			failureReason = err.Error()
		}
	}

	usage := o.Budget.Snapshot(runID)
	usage.Runtime += time.Since(startedAt)

	stopIdleWatch()

	if finalStatus == core.RunCanceled && errors.Is(context.Cause(ctx), errShutdown) {
		o.logErr(ctx, "update run usage", o.Store.UpdateRunUsage(context.Background(), in.TenantID, runID, usage), "run_id", runID)
		o.LoopDet.Reset(runID)
		o.Budget.Forget(runID)
		o.mu.Lock()
		delete(o.runs, runID)
		o.mu.Unlock()
		log.InfoContext(ctx, "run checkpointed for shutdown", "status_left", finalStatus)
		return
	}

	cur, err := o.Store.GetRun(context.Background(), in.TenantID, runID)
	if err == nil {
		cur.Status = finalStatus
		cur.Usage = usage
		cur.FailureReason = failureReason
		if finalStatus != core.RunPaused {
			cur.EndedAt = time.Now().UTC()
		}
		cur.Output, _ = json.Marshal(stageOutputs)
		err = o.Store.UpdateRun(context.Background(), cur)
	}
	o.logErr(ctx, "persist final run state", err, "run_id", runID)
	finalStored := err == nil
	if finalStatus == core.RunFailed || finalStatus == core.RunTimedOut {
		if runErr == nil {
			runErr = errors.New(failureReason)
		}
		o.recordError(context.Background(), in, runID, "", core.VisibilityParticipant, runErr, map[string]any{"scope": "run"})
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
	o.Metrics.Add("agentloop_runs_finished_total", 1, "status", string(finalStatus))
	o.Metrics.Observe("agentloop_run_duration_seconds", usage.Runtime.Seconds())
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

func (o *Orchestrator) runParallelStage(ctx context.Context, runID core.RunID, in StartRunInput, p *plan, i int, vars map[string]string, feedback string) (string, error) {
	stage := p.stages[i]
	prompts := make([]string, len(stage.Agents))
	for j, step := range stage.Agents {
		prompts[j] = render(step.Prompt, in.Input, vars, feedback)
	}
	if err := o.Budget.Reserve(runID, core.Usage{SubAgents: int64(in.reuse.toRun(stage.Name, stage.Agents, prompts))}); err != nil {
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
	sem := make(chan struct{}, in.Limits.MaxConcurrent)
	var wg sync.WaitGroup
	for j, step := range stage.Agents {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				results <- result{idx: j, err: gctx.Err()}
				return
			}
			defer func() { <-sem }()
			out, err := o.runAgentStep(gctx, runID, in, p, i, step, prompts[j], true)
			results <- result{idx: j, out: out, err: err}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	policy := stage.FailurePolicy
	if policy == "" {
		policy = FailFast
	}
	collected := make([]result, 0, len(stage.Agents))
	failures := 0
	var stageErr error
	for r := range results {
		collected = append(collected, r)
		if r.err == nil || stageErr != nil {
			continue
		}
		failures++
		switch {
		case policy == FailFast, errors.Is(r.err, errPaused):
			stageErr = r.err
		case policy == Quorum && failures > len(stage.Agents)-stage.Quorum:
			stageErr = fmt.Errorf("stage %q: %d of %d agents failed, so fewer than the quorum of %d can succeed: %w", stage.Name, failures, len(stage.Agents), stage.Quorum, r.err)
		default:
			continue
		}
		cancel()
	}
	if stageErr != nil {
		return "", stageErr
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sort.Slice(collected, func(i, j int) bool { return collected[i].idx < collected[j].idx })

	var firstErr error
	parts := make([]string, 0, len(collected))
	for _, r := range collected {
		if r.err != nil {
			firstErr = cmp.Or(firstErr, r.err)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", stage.Agents[r.idx].Name, r.out))
		vars[stage.Name+"."+stage.Agents[r.idx].Name] = r.out
	}
	if policy == WaitAll && firstErr != nil || len(parts) == 0 {
		return "", firstErr
	}
	return strings.Join(parts, "\n"), nil
}

// inputImageKind is the artifact kind that stores a run's input images.
const inputImageKind = "input_image"

// taskSpec is one agent task: a workflow step, or a task a parent task delegated.
type taskSpec struct {
	stage     string
	agent     Agent
	prompt    string
	images    []model.Image
	parent    core.TaskID
	spawnedBy core.PrincipalID // the delegating agent; empty for a stage task
	depth     int
	retry     RetryPolicy
	schema    *jsonschema.Schema
	rawSchema json.RawMessage
}

// runAgentStep runs a stage's agent step; reserved means the caller has
// already reserved the sub-agent budget for its first attempt.
func (o *Orchestrator) runAgentStep(ctx context.Context, runID core.RunID, in StartRunInput, p *plan, i int, step AgentStep, prompt string, reserved bool) (string, error) {
	agent, ok := o.Agents[step.Name]
	if !ok {
		return "", fmt.Errorf("unknown agent: %s", step.Name)
	}
	stage := p.stages[i]
	var images []model.Image
	for _, name := range step.Images {
		images = append(images, in.Images[name])
	}
	return o.runTask(ctx, runID, in, taskSpec{
		stage:     stage.Name,
		agent:     agent,
		prompt:    prompt,
		images:    images,
		retry:     stage.Retry,
		schema:    p.output[i],
		rawSchema: stage.OutputSchema,
	}, reserved)
}

// runTask runs spec, or reuses the output of a run it resumes, retrying as
// spec.retry allows. Every attempt is a task that counts as a sub-agent.
func (o *Orchestrator) runTask(ctx context.Context, runID core.RunID, in StartRunInput, spec taskSpec, reserved bool) (string, error) {
	if out, ok := in.reuse.take(spec.stage, spec.agent.Name, spec.prompt); ok {
		return out, nil
	}
	if !reserved {
		if err := o.Budget.ReserveSubAgent(runID); err != nil {
			return "", err
		}
	}
	backoff := spec.retry.Backoff
	for attempt := 1; ; attempt++ {
		out, taskID, err := o.attemptTask(ctx, runID, in, spec, attempt)
		if err == nil {
			return out, nil
		}
		retry := spec.retries(ctx, attempt, err)
		if retry {
			if stalled, n := o.LoopDet.ObserveRetry(runID); stalled {
				storm := fmt.Errorf("%w: %d retries within %s, the last after: %w", errLoopDetected, n, o.LoopDet.RetryWindow, err)
				if lerr := o.onLoop(ctx, in.TenantID, in.SessionID, runID, "", "", map[string]any{"kind": "retry_storm", "retries": n, "agent": spec.agent.Name}, storm); lerr != nil {
					retry, err = false, lerr
				}
			}
		}
		if retry {
			if rerr := o.Budget.ReserveSubAgent(runID); rerr != nil {
				retry, err = false, rerr
			}
		}
		if taskID != "" && ctx.Err() == nil {
			o.recordError(ctx, in, runID, taskID, core.VisibilityInternal, err, map[string]any{
				"scope": "task", "agent": spec.agent.Name, "stage": spec.stage, "attempt": attempt, "will_retry": retry,
			})
		}
		if !retry {
			return "", err
		}
		o.Metrics.Add("agentloop_task_retries_total", 1, "agent", spec.agent.Name)
		o.systemEvent(ctx, in, runID, "", map[string]any{
			"event": "task_retry_scheduled", "agent": spec.agent.Name, "stage": spec.stage,
			"attempt": attempt + 1, "error": err.Error(),
		})
		select {
		case <-ctx.Done():
			o.Budget.Release(runID, core.Usage{SubAgents: 1})
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// retries reports whether a failed attempt is retried.
func (spec taskSpec) retries(ctx context.Context, attempt int, err error) bool {
	return attempt < spec.retry.MaxAttempts && ctx.Err() == nil && !errors.Is(err, core.ErrBudgetExceeded) && !errors.Is(err, model.ErrUnsupported) &&
		!errors.Is(err, errLoopDetected) && !errors.Is(err, core.ErrPolicyDenied)
}

func (o *Orchestrator) attemptTask(ctx context.Context, runID core.RunID, in StartRunInput, spec taskSpec, attempt int) (string, core.TaskID, error) {
	taskID, err := o.Store.CreateTask(ctx, store.Task{
		TenantID:     in.TenantID,
		RunID:        runID,
		ParentTaskID: spec.parent,
		AgentName:    spec.agent.Name,
		Status:       core.TaskRunning,
		Depth:        spec.depth,
		Attempt:      attempt,
		Input:        mustJSON(map[string]any{"prompt": spec.prompt, "stage": spec.stage}),
	})
	if err != nil {
		o.Budget.Release(runID, core.Usage{SubAgents: 1})
		return "", "", err
	}
	o.setTaskActor(runID, taskID, core.AgentPrincipal(spec.agent.Name))
	o.emit(ctx, core.Event{
		TenantID:      in.TenantID,
		SessionID:     in.SessionID,
		RunID:         runID,
		TaskID:        taskID,
		Type:          core.EvTaskSpawned,
		Visibility:    core.VisibilityInternal,
		ActorID:       cmp.Or(spec.spawnedBy, core.SystemPrincipal),
		ParentEventID: o.spawnedEvent(runID, spec.parent),
		Payload: mustJSON(map[string]any{
			"agent": spec.agent.Name, "stage": spec.stage, "prompt": spec.prompt,
			"parent_task_id": spec.parent, "depth": spec.depth, "attempt": attempt,
		}),
	})
	ctx, span := o.startSpan(ctx, "task "+spec.agent.Name,
		attribute.String("agentloop.task_id", string(taskID)),
		attribute.String("agentloop.parent_task_id", string(spec.parent)),
		attribute.String("agentloop.stage", spec.stage),
		attribute.Int("agentloop.depth", spec.depth),
		attribute.Int("agentloop.attempt", attempt))
	actx := ctx
	if spec.agent.Timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, spec.agent.Timeout)
		defer cancel()
	}
	out, err := o.agentLoop(actx, runID, in, spec, taskID)
	if err != nil && actx.Err() != nil && ctx.Err() == nil {
		err = fmt.Errorf("agent %s timed out after %s: %w", spec.agent.Name, spec.agent.Timeout, context.DeadlineExceeded)
	}
	endSpan(span, err)
	if err != nil {
		return "", taskID, o.failTask(ctx, in.TenantID, taskID, err)
	}
	return out, taskID, nil
}

// agentLoop calls the model and the tools it asks for until the model answers.
func (o *Orchestrator) agentLoop(ctx context.Context, runID core.RunID, in StartRunInput, spec taskSpec, taskID core.TaskID) (string, error) {
	agent := spec.agent
	messages := []model.Message{}
	if agent.Instructions != "" {
		messages = append(messages, model.Message{Role: model.RoleSystem, Content: agent.Instructions})
	}
	messages = append(messages, in.history...)
	messages = append(messages, model.Message{Role: model.RoleUser, Content: spec.prompt, Images: spec.images})

	toolSpecs := []model.ToolSpec{}
	allowed := map[string]allowedTool{}
	policies := []policy.ToolPolicy{o.Policy}
	if in.Workflow.ToolPolicy != nil {
		policies = append(policies, *in.Workflow.ToolPolicy)
	}
	for _, name := range agent.Tools {
		def, _, ok := o.Tools.Get(name)
		if !ok {
			continue
		}
		ok2, requiresApproval, reason := policy.AuthorizeAll(def, policies...)
		if !ok2 {
			o.systemEvent(ctx, in, runID, taskID, map[string]any{"event": "tool_denied", "tool": def.Name, "reason": reason})
			continue
		}
		allowed[def.Name] = allowedTool{def: def, requiresApproval: requiresApproval && !o.skipApprovals, approvalReason: reason}
		toolSpecs = append(toolSpecs, model.ToolSpec{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.InputSchema,
		})
	}
	var delegate tool.Executor
	if len(agent.Children) > 0 && spec.depth < in.Limits.MaxDepth {
		toolSpecs = append(toolSpecs, o.delegateSpec(agent))
		delegate = tool.ExecutorFunc(func(ctx context.Context, call tool.Call) (tool.Result, error) {
			var req struct {
				Agent string `json:"agent"`
				Task  string `json:"task"`
			}
			if err := json.Unmarshal(call.Input, &req); err != nil {
				return tool.Result{}, err
			}
			child, ok := o.Agents[req.Agent]
			if !ok || !slices.Contains(agent.Children, req.Agent) {
				return tool.Result{}, fmt.Errorf("%w: %s may not delegate to %q", core.ErrPolicyDenied, agent.Name, req.Agent)
			}
			scope := "delegate:" + string(taskID)
			if stalled, n := o.LoopDet.ObserveReply(runID, scope, req.Agent+" "+req.Task); stalled {
				err := fmt.Errorf("%w: agent %s delegated %d near-identical tasks", errLoopDetected, agent.Name, n)
				if err := o.onLoop(ctx, in.TenantID, in.SessionID, runID, taskID, scope, map[string]any{"kind": "repeated_delegation", "agent": agent.Name, "count": n}, err); err != nil {
					return tool.Result{}, err
				}
			}
			out, err := o.runTask(ctx, runID, in, taskSpec{
				stage: spec.stage, agent: child, prompt: req.Task, parent: taskID, spawnedBy: core.AgentPrincipal(agent.Name), depth: spec.depth + 1,
			}, false)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: mustJSON(map[string]string{"agent": req.Agent, "answer": out})}, nil
		})
	}

	if len(spec.images) > 0 || len(toolSpecs) > 0 {
		caps, err := agent.Provider.Capabilities(ctx, agent.Model)
		if err != nil {
			return "", fmt.Errorf("%w capabilities: %w", errModelCall, err)
		}
		if len(spec.images) > 0 && !caps.Vision {
			return "", fmt.Errorf("agent %s: model %s does not accept images: %w", agent.Name, agent.Model, model.ErrUnsupported)
		}
		if len(toolSpecs) > 0 && !caps.ToolCalling {
			return "", fmt.Errorf("agent %s: model %s does not call tools: %w", agent.Name, agent.Model, model.ErrUnsupported)
		}
	}

	for range agent.MaxToolLoops {
		resp, err := o.modelCall(ctx, runID, in, taskID, agent, model.ChatRequest{
			Messages:       messages,
			Tools:          toolSpecs,
			ResponseSchema: spec.rawSchema,
		})
		if err != nil {
			return "", err
		}
		messages = append(messages, resp.Message)
		if len(resp.ToolCalls) > 0 {
			toolMessages, err := o.runToolCalls(ctx, in.TenantID, in.SessionID, runID, taskID, resp.ToolCalls, allowed, delegate, in.Limits.MaxConcurrent)
			if err != nil {
				return "", err
			}
			messages = append(messages, toolMessages...)
			continue
		}
		content := resp.Message.Content
		if spec.schema != nil {
			if err := validateJSON(spec.schema, content); err != nil {
				o.systemEvent(ctx, in, runID, taskID, map[string]any{"event": "output_rejected", "agent": agent.Name, "error": err.Error()})
				if stalled, count := o.LoopDet.ObserveReply(runID, string(taskID), content); stalled {
					err := fmt.Errorf("%w: agent %s gave %d near-identical rejected answers", errLoopDetected, agent.Name, count)
					if err := o.onLoop(ctx, in.TenantID, in.SessionID, runID, taskID, string(taskID), map[string]any{"kind": "rejected_answers", "agent": agent.Name, "replies": count}, err); err != nil {
						return "", err
					}
				}
				messages = append(messages, model.Message{
					Role:    model.RoleUser,
					Content: "Your answer does not match the required output schema: " + err.Error() + "\nReply with only a JSON document that matches it.",
				})
				continue
			}
		}
		o.completeTask(ctx, in, runID, taskID, agent.Name, content)
		return content, nil
	}
	return "", fmt.Errorf("agent %s exceeded MaxToolLoops=%d", agent.Name, agent.MaxToolLoops)
}

// modelCall makes one budgeted, traced and recorded model call for agent,
// filling the request's IDs and model.
func (o *Orchestrator) modelCall(ctx context.Context, runID core.RunID, in StartRunInput, taskID core.TaskID, agent Agent, req model.ChatRequest) (model.ChatResponse, error) {
	if err := o.checkCost(ctx, runID, in, taskID); err != nil {
		return model.ChatResponse{}, err
	}
	if err := o.Budget.ReserveModelCall(runID); err != nil {
		return model.ChatResponse{}, err
	}
	stepID := core.StepID(core.NewID())
	req.TenantID, req.RunID, req.StepID, req.Model = in.TenantID, runID, stepID, agent.Model
	// ponytail: the whole request is stored per call, so a task's stored history
	// grows quadratically with its tool loops; store deltas if that ever matters.
	o.emit(ctx, core.Event{
		TenantID:      in.TenantID,
		SessionID:     in.SessionID,
		RunID:         runID,
		TaskID:        taskID,
		StepID:        stepID,
		Type:          core.EvModelStarted,
		Visibility:    core.VisibilityInternal,
		ActorID:       core.AgentPrincipal(agent.Name),
		Payload:       mustJSON(map[string]any{"model": agent.Model, "agent": agent.Name}),
		HiddenPayload: mustJSON(map[string]any{"request": req}),
	})

	releaseAgent, err := o.sems.Acquire(ctx, "agent:"+agent.Name, o.Concurrency.PerAgent)
	if err != nil {
		return model.ChatResponse{}, err
	}
	callStart := time.Now()
	cctx, span := o.startSpan(ctx, "chat "+agent.Model,
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", agent.Model),
		attribute.String("agentloop.step_id", string(stepID)))
	resp, err := o.chat(cctx, agent, req, Delta{RunID: runID, TaskID: taskID, StepID: stepID, Agent: agent.Name})
	span.SetAttributes(
		attribute.Int64("gen_ai.usage.input_tokens", resp.Usage.PromptTokens),
		attribute.Int64("gen_ai.usage.output_tokens", resp.Usage.CompletionTokens),
		attribute.Int("agentloop.tool_calls", len(resp.ToolCalls)))
	endSpan(span, err)
	releaseAgent()
	o.Metrics.Observe("agentloop_model_call_duration_seconds", time.Since(callStart).Seconds(), "model", agent.Model)
	if err != nil {
		o.Metrics.Add("agentloop_model_calls_total", 1, "model", agent.Model, "outcome", "error")
		return model.ChatResponse{}, fmt.Errorf("%w: %w", errModelCall, err)
	}
	o.Metrics.Add("agentloop_model_calls_total", 1, "model", agent.Model, "outcome", "ok")
	o.Metrics.Add("agentloop_tokens_total", float64(resp.Usage.PromptTokens), "model", agent.Model, "type", "prompt")
	o.Metrics.Add("agentloop_tokens_total", float64(resp.Usage.CompletionTokens), "model", agent.Model, "type", "completion")
	o.Metrics.Add("agentloop_cost_units_total", float64(resp.Usage.CostUnits), "model", agent.Model)
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
		ActorID:       core.AgentPrincipal(agent.Name),
		Payload:       mustJSON(map[string]any{"usage": resp.Usage, "tool_calls": len(resp.ToolCalls)}),
		HiddenPayload: mustJSON(map[string]any{"content": resp.Message.Content, "thinking": resp.Message.Thinking, "response": withoutRaw(resp)}),
	})
	commitErr := o.Budget.Commit(runID, usage)
	o.persistUsage(ctx, in.TenantID, runID)
	return resp, commitErr
}

func withoutRaw(r model.ChatResponse) model.ChatResponse {
	r.Raw = nil
	return r
}

// chat streams the reply to delta subscribers when the provider can stream.
func (o *Orchestrator) chat(ctx context.Context, agent Agent, req model.ChatRequest, d Delta) (model.ChatResponse, error) {
	s, ok := agent.Provider.(model.Streamer)
	if !ok {
		return agent.Provider.Chat(ctx, req)
	}
	return s.ChatStream(ctx, req, func(md model.Delta) {
		if md.Content != "" && d.TaskID != "" {
			d.Content = md.Content
			o.deltas.publish(d)
		}
	})
}

func (o *Orchestrator) completeTask(ctx context.Context, in StartRunInput, runID core.RunID, taskID core.TaskID, agent, content string) {
	o.emit(ctx, core.Event{
		TenantID:   in.TenantID,
		SessionID:  in.SessionID,
		RunID:      runID,
		TaskID:     taskID,
		Type:       core.EvAgentMessage,
		Visibility: core.VisibilityPublic,
		Payload:    mustJSON(map[string]any{"agent": agent, "content": content}),
	})
	o.logErr(ctx, "add message", o.Store.AddMessage(ctx, store.MessageProjection{
		SessionID:  in.SessionID,
		RunID:      runID,
		Role:       "assistant:" + agent,
		Visibility: core.VisibilityPublic,
		Content:    content,
	}), "run_id", runID)
	o.logErr(ctx, "update task", o.Store.UpdateTask(ctx, store.Task{
		ID:        taskID,
		TenantID:  in.TenantID,
		RunID:     runID,
		AgentName: agent,
		Status:    core.TaskCompleted,
		Output:    mustJSON(map[string]any{"content": content}),
	}), "run_id", runID, "task_id", taskID)
	o.emit(ctx, core.Event{
		TenantID:   in.TenantID,
		SessionID:  in.SessionID,
		RunID:      runID,
		TaskID:     taskID,
		Type:       core.EvTaskCompleted,
		Visibility: core.VisibilityInternal,
		Payload:    mustJSON(map[string]any{"agent": agent}),
	})
}

// delegateTool is the built-in tool an agent with children calls to hand
// them tasks. It is also its tool ID, which no registered tool can have.
const delegateTool = "delegate"

func (o *Orchestrator) delegateSpec(agent Agent) model.ToolSpec {
	desc := "Give a task to another agent and get its answer. The agents you can give tasks to:"
	for _, name := range agent.Children {
		desc += "\n- " + name
		if role := o.Agents[name].Role; role != "" {
			desc += ": " + role
		}
	}
	return model.ToolSpec{
		Name:        delegateTool,
		Description: desc,
		InputSchema: mustJSON(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent": map[string]any{"type": "string", "enum": agent.Children},
				"task":  map[string]any{"type": "string", "description": "everything the agent needs to know to do the task"},
			},
			"required": []string{"agent", "task"},
		}),
	}
}

func (o *Orchestrator) systemEvent(ctx context.Context, in StartRunInput, runID core.RunID, taskID core.TaskID, payload map[string]any) {
	o.emit(ctx, core.Event{
		TenantID:   in.TenantID,
		SessionID:  in.SessionID,
		RunID:      runID,
		TaskID:     taskID,
		Type:       core.EvSystem,
		Visibility: core.VisibilityInternal,
		Payload:    mustJSON(payload),
	})
}

var (
	errModelCall    = errors.New("model")
	errLoopDetected = errors.New("loop detected")
	errPaused       = errors.New("run paused")
)

// LoopResponse is what a detected runaway loop does to its run.
type LoopResponse string

const (
	// LoopFail fails the run; it is the default.
	LoopFail LoopResponse = "fail"
	// LoopPause leaves the run paused until it is resumed or canceled.
	LoopPause LoopResponse = "pause"
	// LoopApprove asks a human whether the run may carry on, and fails it unless approved.
	LoopApprove LoopResponse = "approve"
)

// onLoop records a detected loop, with detail, and answers it as LoopResponse
// says; nil lets the run carry on, with what the detector saw under key forgotten.
func (o *Orchestrator) onLoop(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID, key string, detail map[string]any, err error) error {
	response := cmp.Or(o.LoopResponse, LoopFail)
	o.emit(ctx, core.Event{
		TenantID:   tenant,
		SessionID:  session,
		RunID:      runID,
		TaskID:     taskID,
		Type:       core.EvSystem,
		Visibility: core.VisibilityAudit,
		Payload:    mustJSON(withFields(detail, map[string]any{"event": "loop_detected", "error": err.Error(), "response": response})),
	})
	switch response {
	case LoopPause:
		return fmt.Errorf("%w: %w", errPaused, err)
	case LoopApprove:
		subject := withFields(detail, map[string]any{"gate": "loop", "error": err.Error()})
		status, aerr := o.awaitApproval(ctx, tenant, session, runID, taskID, core.StepID(core.NewID()), "runaway loop detected", subject, mustJSON(subject))
		if aerr != nil {
			return aerr
		}
		if status != "approved" {
			return fmt.Errorf("%w; carrying on was not approved (%s)", err, status)
		}
		o.LoopDet.Clear(runID, key)
		return nil
	}
	return err
}

// costGate holds a run's one answer to whether it may go past ApproveAbove.
type costGate struct {
	mu       sync.Mutex
	asking   chan struct{} // closed when the caller asking a human is done; nil when nobody asks
	answered bool
	err      error
}

// checkCost asks once per execution whether a run past ApproveAbove may call
// models again; other callers wait for that answer or their own context. When
// the asker's context ends first, the next caller asks again.
func (o *Orchestrator) checkCost(ctx context.Context, runID core.RunID, in StartRunInput, taskID core.TaskID) error {
	limit := o.ApproveAbove
	if limit.TotalTokens <= 0 && limit.CostUnits <= 0 || o.skipApprovals {
		return nil
	}
	used := o.Budget.Snapshot(runID)
	if !(limit.TotalTokens > 0 && used.TotalTokens > limit.TotalTokens || limit.CostUnits > 0 && used.CostUnits > limit.CostUnits) {
		return nil
	}
	o.mu.Lock()
	st := o.runs[runID]
	o.mu.Unlock()
	if st == nil {
		return nil
	}
	g := &st.cost
	g.mu.Lock()
	for !g.answered && g.asking != nil {
		asking := g.asking
		g.mu.Unlock()
		select {
		case <-asking:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.mu.Lock()
	}
	if g.answered {
		defer g.mu.Unlock()
		return g.err
	}
	asking := make(chan struct{})
	g.asking = asking
	g.mu.Unlock()

	subject := map[string]any{"gate": "cost", "total_tokens": used.TotalTokens, "cost_units": used.CostUnits,
		"approve_above": map[string]int64{"total_tokens": limit.TotalTokens, "cost_units": limit.CostUnits}}
	status, err := o.awaitApproval(ctx, in.TenantID, in.SessionID, runID, taskID, core.StepID(core.NewID()), "run passed its cost approval threshold", subject, mustJSON(subject))
	g.mu.Lock()
	defer g.mu.Unlock()
	defer close(asking)
	g.asking = nil
	if err != nil {
		return err
	}
	g.answered = true
	if status != "approved" {
		g.err = fmt.Errorf("%w: going past the cost approval threshold was not approved (%s)", core.ErrPolicyDenied, status)
	}
	return g.err
}

// errorCode classifies err for error_recorded events.
func errorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
	case errors.Is(err, core.ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.Is(err, core.ErrPolicyDenied):
		return "policy_denied"
	case errors.Is(err, model.ErrUnsupported):
		return "unsupported_capability"
	case errors.Is(err, errPaused):
		return "paused"
	case errors.Is(err, errLoopDetected):
		return "loop_detected"
	case errors.Is(err, errModelCall):
		return "provider_unavailable"
	default:
		return "failed"
	}
}

// recordError emits an error_recorded event for err with fields.
func (o *Orchestrator) recordError(ctx context.Context, in StartRunInput, runID core.RunID, taskID core.TaskID, vis core.Visibility, err error, fields map[string]any) {
	o.emit(context.WithoutCancel(ctx), core.Event{
		TenantID:   in.TenantID,
		SessionID:  in.SessionID,
		RunID:      runID,
		TaskID:     taskID,
		Type:       core.EvError,
		Visibility: vis,
		Payload:    mustJSON(withFields(fields, map[string]any{"code": errorCode(err), "message": err.Error()})),
	})
}

func (o *Orchestrator) failTask(ctx context.Context, tenant core.TenantID, taskID core.TaskID, err error) error {
	ctx = context.WithoutCancel(ctx)
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

func (o *Orchestrator) runToolCalls(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID, calls []model.ToolCall, allowed map[string]allowedTool, delegate tool.Executor, maxParallel int) ([]model.Message, error) {
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
			err := fmt.Errorf("%w on tool %s after %d identical calls", errLoopDetected, tc.Name, count)
			key := policy.ToolKey(tc.Name, hashInput(tc.Input))
			if err := o.onLoop(ctx, tenant, session, runID, taskID, key, map[string]any{"kind": "repeated_tool_call", "tool": tc.Name, "count": count}, err); err != nil {
				return nil, err
			}
		}
		callRec := tool.Call{
			TenantID: tenant,
			RunID:    runID,
			TaskID:   taskID,
			StepID:   core.StepID(core.NewID()),
			Tool:     core.ToolDefinition{Name: tc.Name},
			Input:    tc.Input,
		}
		if tc.Name == delegateTool && delegate != nil {
			callRec.Tool = core.ToolDefinition{ID: delegateTool, Name: delegateTool}
			plans = append(plans, plannedTool{idx: i, tc: tc, call: callRec, exec: delegate, runParallel: true})
			continue
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
		if err := o.Tools.CheckInput(tc.Name, tc.Input); err != nil {
			callRec.Tool = allowedTool.def
			plans = append(plans, plannedTool{idx: i, tc: tc, call: callRec, refused: fmt.Errorf("invalid input for tool %s: %w", tc.Name, err)})
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
	group := make(chan struct{}, max(maxParallel, 1))
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
			call := plans[i].call
			request := mustJSON(map[string]any{"tool": call.Tool.Name, "input": json.RawMessage(call.Input)})
			status, err := o.awaitApproval(gctx, tenant, session, runID, taskID, call.StepID, plans[i].approvalReason, map[string]any{"tool": call.Tool.Name}, request)
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
			select {
			case group <- struct{}{}:
			case <-gctx.Done():
				results <- toolOut{idx: plan.idx, err: gctx.Err()}
				continue
			}
			wg.Add(1)
			go func(plan plannedTool) {
				defer wg.Done()
				defer func() { <-group }()
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

// awaitApproval records an approval request about subject, which its events
// carry, and waits until it is resolved, expires or ctx ends.
func (o *Orchestrator) awaitApproval(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID, stepID core.StepID, reason string, subject map[string]any, request json.RawMessage) (string, error) {
	if reason == "" {
		reason = "tool requires approval"
	}
	ctx, span := o.startSpan(ctx, "await_approval", attribute.String("agentloop.step_id", string(stepID)))
	defer span.End()
	var expiresAt time.Time
	if o.ApprovalTimeout > 0 {
		expiresAt = time.Now().UTC().Add(o.ApprovalTimeout)
	}
	approvalID, err := o.Approvals.CreateApproval(ctx, store.Approval{
		TenantID:  tenant,
		RunID:     runID,
		StepID:    stepID,
		Reason:    reason,
		ExpiresAt: expiresAt,
		Request:   request,
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
		StepID:     stepID,
		Type:       core.EvApprovalReq,
		Visibility: core.VisibilityParticipant,
		Payload:    mustJSON(withFields(subject, map[string]any{"approval_id": approvalID, "reason": reason, "expires_at": expiresAt})),
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
		StepID:     stepID,
		Type:       core.EvApprovalRes,
		Visibility: core.VisibilityParticipant,
		ActorID:    cmp.Or(approval.ResolvedBy, core.SystemPrincipal),
		Payload:    mustJSON(withFields(subject, map[string]any{"approval_id": approvalID, "status": approval.Status, "resolved_by": approval.ResolvedBy})),
	})
	if ctx.Err() != nil {
		return "", fmt.Errorf("approval %s: %w", approvalID, ctx.Err())
	}
	o.transitionRun(ctx, runID, tenant, core.RunRunning, "")
	return approval.Status, nil
}

func withFields(base, extra map[string]any) map[string]any {
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

// closeApproval resolves an open approval with status, or returns the
// decision a human recorded first.
func (o *Orchestrator) closeApproval(ctx context.Context, tenant core.TenantID, id core.ApprovalID, status string) (store.Approval, error) {
	if err := o.Approvals.ResolveApproval(ctx, tenant, id, status, "", nil); err != nil && !errors.Is(err, core.ErrConflict) {
		return store.Approval{}, err
	}
	return o.Approvals.GetApproval(ctx, tenant, id)
}

func (o *Orchestrator) invokeTool(ctx context.Context, exec tool.Executor, call tool.Call, tc model.ToolCall, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID) (_ model.Message, err error) {
	ctx, span := o.startSpan(ctx, "execute_tool "+call.Tool.Name,
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", call.Tool.Name),
		attribute.String("agentloop.step_id", string(call.StepID)))
	defer func() { endSpan(span, err) }()
	// A delegating task holding a slot its descendants wait for would deadlock.
	if call.Tool.ID != delegateTool {
		limit := o.Concurrency.PerTool
		if call.Tool.Concurrency == core.ConcurrencyExclusive {
			limit = 1
		}
		releaseTool, err := o.sems.Acquire(ctx, "tool:"+call.Tool.Name, limit)
		if err != nil {
			return model.Message{}, fmt.Errorf("tool %s: %w", call.Tool.Name, err)
		}
		defer releaseTool()
	}
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
	callStart := time.Now()
	tctx, cancel := ctx, context.CancelFunc(func() {})
	if call.Tool.Timeout > 0 {
		tctx, cancel = context.WithTimeout(ctx, call.Tool.Timeout)
	}
	res, err := safeExecute(tctx, exec, call)
	cancel()
	if err != nil {
		span.RecordError(err)
	}
	o.Metrics.Observe("agentloop_tool_call_duration_seconds", time.Since(callStart).Seconds(), "tool", call.Tool.Name)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, core.ErrBudgetExceeded) || errors.Is(err, errLoopDetected) {
			o.toolFailed(ctx, tenant, session, runID, taskID, call, tc, err)
			return model.Message{}, fmt.Errorf("tool %s: %w", call.Tool.Name, err)
		}
		return o.toolFailed(ctx, tenant, session, runID, taskID, call, tc, err), nil
	}
	artifacts, err := o.storeArtifacts(ctx, tenant, session, runID, taskID, call, res.Artifacts)
	if err != nil {
		if ctx.Err() != nil {
			o.toolFailed(ctx, tenant, session, runID, taskID, call, tc, err)
			return model.Message{}, fmt.Errorf("tool %s: %w", call.Tool.Name, err)
		}
		return o.toolFailed(ctx, tenant, session, runID, taskID, call, tc, err), nil
	}
	o.Metrics.Add("agentloop_tool_calls_total", 1, "tool", call.Tool.Name, "outcome", "ok")
	completed := map[string]any{"tool": call.Tool.Name, "output": json.RawMessage(res.Output)}
	if len(artifacts) > 0 {
		completed["artifacts"] = artifacts
	}
	o.emit(ctx, core.Event{
		TenantID:   tenant,
		SessionID:  session,
		RunID:      runID,
		TaskID:     taskID,
		StepID:     call.StepID,
		Type:       core.EvToolCompleted,
		Visibility: core.VisibilityInternal,
		Payload:    mustJSON(completed),
	})
	return model.Message{
		Role:       model.RoleTool,
		Name:       call.Tool.Name,
		ToolCallID: tc.ID,
		Content:    string(res.Output),
	}, nil
}

// storeArtifacts stores what a tool call produced, each with an
// artifact_created event, and returns their IDs.
func (o *Orchestrator) storeArtifacts(ctx context.Context, tenant core.TenantID, session core.SessionID, runID core.RunID, taskID core.TaskID, call tool.Call, produced []tool.Artifact) ([]string, error) {
	for _, p := range produced {
		if err := o.checkArtifactSize(cmp.Or(p.Name, call.Tool.Name), len(p.Content)); err != nil {
			return nil, err
		}
	}
	var ids []string
	for _, p := range produced {
		a := store.Artifact{
			TenantID: tenant, SessionID: session, RunID: runID, TaskID: taskID, StepID: call.StepID, Producer: call.Tool.Name,
			Kind: cmp.Or(p.Kind, "file"), Name: cmp.Or(p.Name, call.Tool.Name), MimeType: p.MimeType, Content: p.Content, Metadata: p.Metadata,
		}
		id, err := o.Store.PutArtifact(ctx, a)
		if err != nil {
			return nil, fmt.Errorf("store artifact %q: %w", a.Name, err)
		}
		ids = append(ids, id)
		o.emit(ctx, core.Event{
			TenantID:   tenant,
			SessionID:  session,
			RunID:      runID,
			TaskID:     taskID,
			StepID:     call.StepID,
			Type:       core.EvArtifact,
			Visibility: core.VisibilityParticipant,
			Payload: mustJSON(map[string]any{
				"artifact_id": id, "name": a.Name, "kind": a.Kind, "mime_type": cmp.Or(a.MimeType, "application/octet-stream"),
				"size": len(a.Content), "producer": a.Producer,
			}),
		})
	}
	return ids, nil
}

func (o *Orchestrator) checkArtifactSize(name string, size int) error {
	if o.MaxArtifactBytes > 0 && int64(size) > o.MaxArtifactBytes {
		return fmt.Errorf("%s is %d bytes, more than the %d an artifact may hold", name, size, o.MaxArtifactBytes)
	}
	return nil
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
	// A refused call's name comes from model output, so it must not become a label value.
	label := call.Tool.Name
	if call.Tool.ID == "" {
		label = "unknown"
	}
	o.Metrics.Add("agentloop_tool_calls_total", 1, "tool", label, "outcome", "error")
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

// emit appends ev, linking it to the task_spawned event of its task as parent
// and to the previous event of its step as cause.
func (o *Orchestrator) emit(ctx context.Context, ev core.Event) {
	ev.ID = core.EventID(core.NewID())
	o.mu.Lock()
	st := o.runs[ev.RunID]
	o.mu.Unlock()
	if st != nil {
		st.activity.Lock()
		st.lastAt, st.lastEvent = time.Now(), ev.Type
		if ev.Type == core.EvTaskSpawned {
			st.taskSpawned[ev.TaskID] = ev.ID
		} else if ev.TaskID != "" {
			ev.ParentEventID = st.taskSpawned[ev.TaskID]
		}
		if ev.ActorID == "" {
			ev.ActorID = st.taskActor[ev.TaskID]
		}
		if ev.StepID != "" {
			ev.CausationID = string(st.lastInStep[ev.StepID])
			st.lastInStep[ev.StepID] = ev.ID
			if ev.Type == core.EvModelCompleted || ev.Type == core.EvToolCompleted {
				delete(st.lastInStep, ev.StepID)
			}
		}
		st.activity.Unlock()
	}
	o.appendEvent(ctx, ev)
}

func (o *Orchestrator) spawnedEvent(runID core.RunID, task core.TaskID) core.EventID {
	o.mu.Lock()
	st := o.runs[runID]
	o.mu.Unlock()
	if st == nil || task == "" {
		return ""
	}
	st.activity.Lock()
	defer st.activity.Unlock()
	return st.taskSpawned[task]
}

func (o *Orchestrator) setTaskActor(runID core.RunID, task core.TaskID, actor core.PrincipalID) {
	o.mu.Lock()
	st := o.runs[runID]
	o.mu.Unlock()
	if st == nil {
		return
	}
	st.activity.Lock()
	st.taskActor[task] = actor
	st.activity.Unlock()
}

// appendEvent stores ev, by default as the system principal's; the events of
// a run share its ID as correlation ID.
func (o *Orchestrator) appendEvent(ctx context.Context, ev core.Event) {
	if ev.ActorID == "" {
		ev.ActorID = core.SystemPrincipal
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	if ev.CorrelationID == "" && ev.RunID != "" {
		ev.CorrelationID = string(ev.RunID)
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

func (o *Orchestrator) startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if o.Tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return o.Tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
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
