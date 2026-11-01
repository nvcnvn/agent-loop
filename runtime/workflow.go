package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/tool"
)

// Agent is a runtime-resolvable agent definition.
type Agent struct {
	Name         string         `json:"name"`
	Role         string         `json:"role,omitempty"`
	Instructions string         `json:"instructions,omitempty"`
	Model        string         `json:"model,omitempty"` // provider model name
	Provider     model.Provider `json:"-"`
	Tools        []string       `json:"tools,omitempty"`    // tool names from the tool registry the agent may call
	Children     []string       `json:"children,omitempty"` // agents it may hand tasks to with the delegate tool
	MaxToolLoops int            `json:"max_tool_loops,omitempty"`
	// Timeout bounds each attempt of a task the agent runs; zero means no bound.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// StageKind picks between sequential and parallel execution within a stage.
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
	Quorum     FailurePolicy = "quorum"
)

// AgentStep is one agent invocation inside a stage.
type AgentStep struct {
	Name   string   `json:"name"`             // agent name; resolved against the registered agents
	Prompt string   `json:"prompt"`           // template; see render
	Images []string `json:"images,omitempty"` // names of run input images shown with the prompt
}

// RetryPolicy reruns a failed agent task.
type RetryPolicy struct {
	MaxAttempts int           `json:"max_attempts,omitempty"` // including the first; zero or one means no retry
	Backoff     time.Duration `json:"backoff,omitempty"`      // wait before the first retry, doubled for each later one
}

// HandBack sends work back to an earlier stage when a stage's output fails validation.
type HandBack struct {
	When     string `json:"when"`                // regexp; an output that matches it hands work back
	To       string `json:"to"`                  // the stage to run again, this one or one it depends on
	MaxTimes int    `json:"max_times,omitempty"` // hand-backs before the run fails; zero means 3
}

// Stage is one node of a workflow.
type Stage struct {
	Name          string        `json:"name"`
	Kind          StageKind     `json:"kind,omitempty"`
	Agents        []AgentStep   `json:"agents"`
	FailurePolicy FailurePolicy `json:"failure_policy,omitempty"`
	// Quorum is how many agents of a quorum stage must succeed.
	Quorum int `json:"quorum,omitempty"`
	// DependsOn names the stages that must finish first. When no stage of a
	// workflow sets it, each stage depends on the one before it.
	DependsOn []string      `json:"depends_on,omitempty"`
	Timeout   time.Duration `json:"timeout,omitempty"`
	Retry     RetryPolicy   `json:"retry,omitzero"`
	HandBack  *HandBack     `json:"hand_back,omitempty"`
	// StopWhen is a regexp; when the stage output matches it, no further stage starts and the run succeeds.
	StopWhen string `json:"stop_when,omitempty"`
	// OutputSchema is a JSON Schema each agent's answer in the stage must satisfy.
	OutputSchema  json.RawMessage `json:"output_schema,omitempty"`
	ApproveStart  bool            `json:"approve_start,omitempty"`
	ApproveOutput bool            `json:"approve_output,omitempty"`
}

// Workflow is a graph of stages.
type Workflow struct {
	Name string `json:"name"`
	// InputSchema is a JSON Schema the run input, a JSON object of strings, must satisfy.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// ToolPolicy applies to the workflow's runs on top of Orchestrator.Policy.
	ToolPolicy *policy.ToolPolicy `json:"tool_policy,omitempty"`
	Stages     []Stage            `json:"stages"`
}

const defaultHandBacks = 3

// plan is a validated workflow with its patterns and schemas compiled.
type plan struct {
	stages     []Stage
	deps       [][]int
	dependents [][]int
	input      *jsonschema.Schema
	output     []*jsonschema.Schema
	stopWhen   []*regexp.Regexp
	handWhen   []*regexp.Regexp
	handTo     []int
}

func compileWorkflow(wf Workflow, agents map[string]Agent) (*plan, error) {
	n := len(wf.Stages)
	if n == 0 {
		return nil, errors.New("workflow has no stages")
	}
	p := &plan{
		stages:     wf.Stages,
		deps:       make([][]int, n),
		dependents: make([][]int, n),
		output:     make([]*jsonschema.Schema, n),
		stopWhen:   make([]*regexp.Regexp, n),
		handWhen:   make([]*regexp.Regexp, n),
		handTo:     make([]int, n),
	}
	index := map[string]int{}
	graph := false
	for i, s := range wf.Stages {
		if s.Name == "" {
			return nil, fmt.Errorf("stage %d has no name", i)
		}
		if _, dup := index[s.Name]; dup {
			return nil, fmt.Errorf("duplicate stage %q", s.Name)
		}
		index[s.Name] = i
		graph = graph || len(s.DependsOn) > 0
	}
	var err error
	if wf.ToolPolicy != nil {
		if err := wf.ToolPolicy.Validate(); err != nil {
			return nil, err
		}
	}
	if len(wf.InputSchema) > 0 {
		if p.input, err = tool.CompileSchema(wf.InputSchema); err != nil {
			return nil, fmt.Errorf("input_schema: %w", err)
		}
	}
	for i, s := range wf.Stages {
		bad := func(format string, a ...any) error {
			return fmt.Errorf("stage %q: %s", s.Name, fmt.Sprintf(format, a...))
		}
		switch s.Kind {
		case "", StageSequential, StageParallel:
		default:
			return nil, bad("unknown kind %q", s.Kind)
		}
		switch s.FailurePolicy {
		case "", FailFast, WaitAll, BestEffort, Quorum:
		default:
			return nil, bad("unknown failure_policy %q", s.FailurePolicy)
		}
		if len(s.Agents) == 0 {
			return nil, bad("no agents")
		}
		if (s.FailurePolicy == Quorum) != (s.Quorum != 0) || s.Quorum < 0 || s.Quorum > len(s.Agents) {
			return nil, bad("quorum must be set for, and only for, failure_policy quorum, between 1 and the number of agents")
		}
		if s.FailurePolicy == Quorum && s.Kind != StageParallel {
			return nil, bad("failure_policy quorum needs kind parallel")
		}
		for _, a := range s.Agents {
			if _, ok := agents[a.Name]; !ok {
				return nil, bad("unknown agent %q", a.Name)
			}
		}
		if s.Timeout < 0 || s.Retry.MaxAttempts < 0 || s.Retry.Backoff < 0 {
			return nil, bad("timeout and retry must not be negative")
		}
		if graph {
			for _, d := range s.DependsOn {
				j, ok := index[d]
				if !ok || j == i {
					return nil, bad("depends on unknown stage %q", d)
				}
				p.deps[i] = append(p.deps[i], j)
				p.dependents[j] = append(p.dependents[j], i)
			}
		} else if i > 0 {
			p.deps[i] = []int{i - 1}
			p.dependents[i-1] = []int{i}
		}
		if s.StopWhen != "" {
			if p.stopWhen[i], err = regexp.Compile(s.StopWhen); err != nil {
				return nil, bad("stop_when: %v", err)
			}
		}
		if len(s.OutputSchema) > 0 {
			if p.output[i], err = tool.CompileSchema(s.OutputSchema); err != nil {
				return nil, bad("output_schema: %v", err)
			}
		}
	}
	if cyclic(p.deps) {
		return nil, errors.New("stage dependencies form a cycle")
	}
	for i, s := range wf.Stages {
		if s.HandBack == nil {
			continue
		}
		if p.handWhen[i], err = regexp.Compile(s.HandBack.When); err != nil || s.HandBack.When == "" {
			return nil, fmt.Errorf("stage %q: hand_back.when must be a non-empty regexp", s.Name)
		}
		to, ok := index[s.HandBack.To]
		if !ok || !slices.Contains(p.ancestorsOrSelf(i), to) {
			return nil, fmt.Errorf("stage %q: hand_back.to %q is not this stage or one it depends on", s.Name, s.HandBack.To)
		}
		if s.HandBack.MaxTimes < 0 {
			return nil, fmt.Errorf("stage %q: hand_back.max_times must not be negative", s.Name)
		}
		p.handTo[i] = to
	}
	return p, nil
}

func cyclic(deps [][]int) bool {
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make([]int, len(deps))
	var visit func(int) bool
	visit = func(i int) bool {
		switch state[i] {
		case visiting:
			return true
		case visited:
			return false
		}
		state[i] = visiting
		for _, d := range deps[i] {
			if visit(d) {
				return true
			}
		}
		state[i] = visited
		return false
	}
	for i := range deps {
		if visit(i) {
			return true
		}
	}
	return false
}

func (p *plan) ancestorsOrSelf(i int) []int {
	return p.closure(i, p.deps)
}

func (p *plan) descendantsOrSelf(i int) []int {
	return p.closure(i, p.dependents)
}

func (p *plan) closure(i int, edges [][]int) []int {
	seen := map[int]bool{i: true}
	queue := []int{i}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range edges[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (p *plan) checkInput(input map[string]string) error {
	if p.input == nil {
		return nil
	}
	doc := make(map[string]any, len(input))
	for k, v := range input {
		doc[k] = v
	}
	if err := p.input.Validate(doc); err != nil {
		return fmt.Errorf("input does not match the workflow input_schema: %w", err)
	}
	return nil
}

func validateJSON(s *jsonschema.Schema, text string) error {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
	if err != nil {
		return fmt.Errorf("the answer is not a JSON document: %w", err)
	}
	return s.Validate(doc)
}

// render fills {{input.<key>}}, {{stage.<name>}}, {{stage.<name>.<agent>}}
// and {{feedback}}, the output that last handed work back to this stage.
func render(tmpl string, input, vars map[string]string, feedback string) string {
	pairs := []string{"{{feedback}}", feedback}
	for k, v := range input {
		pairs = append(pairs, "{{input."+k+"}}", v)
	}
	for k, v := range vars {
		pairs = append(pairs, "{{stage."+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

// runStages runs every stage once its dependencies have finished, applying
// hand-backs and stop conditions, and records stage outputs in vars.
func (o *Orchestrator) runStages(ctx context.Context, runID core.RunID, in StartRunInput, p *plan, vars map[string]string) error {
	type finished struct {
		i    int
		out  string
		vars map[string]string
		err  error
	}
	const (
		pending = iota
		running
		done
	)
	n := len(p.stages)
	state := make([]int, n)
	handBacks := make([]int, n)
	feedback := make([]string, n)
	results := make(chan finished)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var failed error
	var stopped bool
	var resets []int
	active := 0
	for {
		if failed == nil && !stopped && len(resets) == 0 {
			for i := range n {
				if state[i] != pending || slices.ContainsFunc(p.deps[i], func(d int) bool { return state[d] != done }) {
					continue
				}
				state[i] = running
				active++
				local := maps.Clone(vars)
				go func() {
					out, err := o.runStage(gctx, runID, in, p, i, local, feedback[i])
					results <- finished{i: i, out: out, vars: local, err: err}
				}()
			}
		}
		if active == 0 {
			if len(resets) == 0 {
				return failed
			}
			for _, t := range resets {
				for _, d := range p.descendantsOrSelf(t) {
					state[d] = pending
				}
			}
			resets = nil
			continue
		}
		r := <-results
		active--
		if failed != nil {
			continue
		}
		if r.err != nil {
			failed = r.err
			cancel()
			continue
		}
		stage := p.stages[r.i]
		state[r.i] = done
		vars[stage.Name] = r.out
		for _, a := range stage.Agents {
			if v, ok := r.vars[stage.Name+"."+a.Name]; ok {
				vars[stage.Name+"."+a.Name] = v
			}
		}
		if re := p.handWhen[r.i]; re != nil && re.MatchString(r.out) {
			handBacks[r.i]++
			limit := stage.HandBack.MaxTimes
			if limit == 0 {
				limit = defaultHandBacks
			}
			to := p.handTo[r.i]
			if handBacks[r.i] > limit {
				failed = fmt.Errorf("stage %q still failed validation after handing work back to %q %d times", stage.Name, stage.HandBack.To, limit)
				cancel()
				continue
			}
			o.systemEvent(ctx, in, runID, "", map[string]any{"event": "stage_handed_back", "stage": stage.Name, "to": stage.HandBack.To, "count": handBacks[r.i]})
			feedback[to] = r.out
			resets = append(resets, to)
			continue
		}
		if re := p.stopWhen[r.i]; re != nil && re.MatchString(r.out) {
			o.systemEvent(ctx, in, runID, "", map[string]any{"event": "workflow_stopped", "stage": stage.Name})
			stopped = true
		}
	}
}

// runStage runs one stage, between its approval gates, and returns its aggregated output.
func (o *Orchestrator) runStage(ctx context.Context, runID core.RunID, in StartRunInput, p *plan, i int, vars map[string]string, feedback string) (string, error) {
	stage := p.stages[i]
	if stage.ApproveStart {
		if err := o.stageGate(ctx, runID, in, stage.Name, "start", ""); err != nil {
			return "", err
		}
	}
	ctx, span := o.startSpan(ctx, "stage "+stage.Name, attribute.String("agentloop.stage", stage.Name))
	defer span.End()
	sctx := ctx
	if stage.Timeout > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(ctx, stage.Timeout)
		defer cancel()
	}
	var out string
	var err error
	if stage.Kind == StageParallel {
		out, err = o.runParallelStage(sctx, runID, in, p, i, vars, feedback)
	} else {
		for _, step := range stage.Agents {
			out, err = o.runAgentStep(sctx, runID, in, p, i, step, render(step.Prompt, in.Input, vars, feedback), false)
			if err != nil {
				break
			}
			vars[stage.Name+"."+step.Name] = out
		}
	}
	if err != nil && sctx.Err() != nil && ctx.Err() == nil {
		out, err = "", fmt.Errorf("stage %q timed out after %s: %w", stage.Name, stage.Timeout, context.DeadlineExceeded)
	}
	if err == nil && stage.ApproveOutput {
		err = o.stageGate(ctx, runID, in, stage.Name, "output", out)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return out, err
}

// stageGate waits for a human to approve starting a stage or its output.
func (o *Orchestrator) stageGate(ctx context.Context, runID core.RunID, in StartRunInput, stage, gate, output string) error {
	subject := map[string]any{"stage": stage, "gate": gate}
	if gate == "output" {
		subject["output"] = output
	}
	status, err := o.awaitApproval(ctx, in.TenantID, in.SessionID, runID, "", core.StepID(core.NewID()), "stage "+gate+" requires approval", subject, mustJSON(subject))
	if err != nil {
		return err
	}
	if status != "approved" {
		return fmt.Errorf("stage %q %s was not approved (%s)", stage, gate, status)
	}
	return nil
}
