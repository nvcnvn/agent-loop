package runtime

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/policy"
)

// Config holds what an operator sets in a file: the agents, named workflow
// templates, history compaction, tool policy, run limits and the worker pool.
type Config struct {
	Agents     []Agent     `json:"agents"`
	Workflows  []Workflow  `json:"workflows,omitempty"`
	Compaction *Compaction `json:"compaction,omitempty"`
	// ToolPolicy applies to every run.
	ToolPolicy *policy.ToolPolicy `json:"tool_policy,omitempty"`
	// LoopResponse and ApproveAbove set the Orchestrator fields of the same names.
	LoopResponse LoopResponse `json:"loop_response,omitempty"`
	ApproveAbove *core.Usage  `json:"approve_above,omitempty"`
	// MaxArtifactBytes, when set, replaces Orchestrator.MaxArtifactBytes.
	MaxArtifactBytes *int64 `json:"max_artifact_bytes,omitempty"`
	// Limits sets the run limits a run gets when it asks for none (Default)
	// and the most it may ask for (Max); unset fields keep the Orchestrator's.
	Limits *LimitsConfig `json:"limits,omitempty"`
	// Concurrency sets the worker pool: runs executing at once, runs waiting for a slot, and the
	// per-tenant, workflow, agent and tool bounds of policy.ConcurrencyLimits.
	Concurrency *ConcurrencyConfig `json:"concurrency,omitempty"`
	// ApprovalTimeout, IdleWarnAfter, MaxHistoryMessages and ToolRetry, when set, replace the
	// Orchestrator fields of the same names.
	ApprovalTimeout    *time.Duration `json:"approval_timeout,omitempty"`
	IdleWarnAfter      *time.Duration `json:"idle_warn_after,omitempty"`
	MaxHistoryMessages *int           `json:"max_history_messages,omitempty"`
	ToolRetry          *RetryPolicy   `json:"tool_retry,omitempty"`
	// Retention sets Orchestrator.Retention.
	Retention *Retention `json:"retention,omitempty"`
}

// LimitsConfig is the run limits a Config sets: Default fills what a run
// does not ask for, Max caps what it may; a zero field keeps the Orchestrator's.
type LimitsConfig struct {
	Default core.Limits `json:"default,omitzero"`
	Max     core.Limits `json:"max,omitzero"`
}

// ConcurrencyConfig is the worker pool and queue a Config sets; an absent or
// zero field keeps the Orchestrator's, except the two run counts, where a
// present zero means no cap on running runs, or no queue.
type ConcurrencyConfig struct {
	MaxActiveRuns *int `json:"max_active_runs,omitempty"`
	MaxQueuedRuns *int `json:"max_queued_runs,omitempty"`
	PerTenant     int  `json:"per_tenant,omitempty"`
	PerWorkflow   int  `json:"per_workflow,omitempty"`
	PerAgent      int  `json:"per_agent,omitempty"`
	PerTool       int  `json:"per_tool,omitempty"`
}

// Apply sets o's workflow templates, compaction, policy, limits and pool
// from c, before o starts its first run; what c leaves unset keeps o's value.
func (c Config) Apply(o *Orchestrator) {
	for _, wf := range c.Workflows {
		o.Workflows[wf.Name] = wf
	}
	if c.Compaction != nil {
		o.Compaction = *c.Compaction
	}
	if c.ToolPolicy != nil {
		o.Policy = *c.ToolPolicy
	}
	o.LoopResponse = c.LoopResponse
	if c.ApproveAbove != nil {
		o.ApproveAbove = *c.ApproveAbove
	}
	if c.MaxArtifactBytes != nil {
		o.MaxArtifactBytes = *c.MaxArtifactBytes
	}
	if l := c.Limits; l != nil {
		o.DefaultLimits = l.Default.Fill(o.DefaultLimits)
		o.MaxLimits = l.Max.Fill(o.MaxLimits)
	}
	if p := c.Concurrency; p != nil {
		if p.MaxActiveRuns != nil {
			o.MaxActiveRuns = *p.MaxActiveRuns
		}
		if p.MaxQueuedRuns != nil {
			o.MaxQueuedRuns = *p.MaxQueuedRuns
		}
		o.Concurrency = policy.ConcurrencyLimits{
			PerTenant: cmp.Or(p.PerTenant, o.Concurrency.PerTenant), PerWorkflow: cmp.Or(p.PerWorkflow, o.Concurrency.PerWorkflow),
			PerAgent: cmp.Or(p.PerAgent, o.Concurrency.PerAgent), PerTool: cmp.Or(p.PerTool, o.Concurrency.PerTool),
		}
	}
	if c.ApprovalTimeout != nil {
		o.ApprovalTimeout = *c.ApprovalTimeout
	}
	if c.IdleWarnAfter != nil {
		o.IdleWarnAfter = *c.IdleWarnAfter
	}
	if c.MaxHistoryMessages != nil {
		o.MaxHistoryMessages = *c.MaxHistoryMessages
	}
	if c.ToolRetry != nil {
		o.ToolRetry = *c.ToolRetry
	}
	if c.Retention != nil {
		o.Retention = *c.Retention
	}
}

var envReference = regexp.MustCompile(`\$?\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces each ${NAME} in a config file with the environment
// variable NAME, as it is, failing on one that is unset; $${NAME} stays as
// ${NAME}, and other $ signs stay as they are.
func ExpandEnv(data []byte) ([]byte, error) {
	var missing []string
	out := envReference.ReplaceAllFunc(data, func(ref []byte) []byte {
		if ref[1] == '$' {
			return ref[1:]
		}
		name := string(envReference.FindSubmatch(ref)[1])
		val, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return []byte(val)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("config uses unset environment variables %v", missing)
	}
	return out, nil
}

// ParseConfig decodes a JSON config and checks its agents and workflows against each other.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// Validate checks the agents, workflows, compaction and tool policy.
func (c Config) Validate() error {
	agents := map[string]Agent{}
	for _, a := range c.Agents {
		if a.Name == "" {
			return errors.New("agent without a name")
		}
		if _, dup := agents[a.Name]; dup {
			return fmt.Errorf("duplicate agent %q", a.Name)
		}
		if a.Timeout < 0 || a.MaxToolLoops < 0 {
			return fmt.Errorf("agent %q: timeout and max_tool_loops must not be negative", a.Name)
		}
		agents[a.Name] = a
	}
	for _, a := range c.Agents {
		for _, child := range a.Children {
			if _, ok := agents[child]; !ok {
				return fmt.Errorf("agent %q: unknown child agent %q", a.Name, child)
			}
		}
	}
	if c := c.Compaction; c != nil {
		if _, ok := agents[c.Agent]; !ok {
			return fmt.Errorf("compaction: unknown agent %q", c.Agent)
		}
		if c.MaxMessages < 0 || c.MaxTokens < 0 || c.KeepRecent < 0 || c.TaskMaxTokens < 0 || c.MaxMessages == 0 && c.MaxTokens == 0 && c.TaskMaxTokens == 0 {
			return errors.New("compaction: needs max_messages, max_tokens or task_max_tokens, and no negative values")
		}
	}
	if c.ToolPolicy != nil {
		if err := c.ToolPolicy.Validate(); err != nil {
			return err
		}
	}
	if a := c.ApproveAbove; a != nil && (a.TotalTokens < 0 || a.CostUnits < 0) {
		return errors.New("approve_above must not be negative")
	}
	switch c.LoopResponse {
	case "", LoopFail, LoopPause, LoopApprove:
	default:
		return fmt.Errorf("loop_response %q is not fail, pause or approve", c.LoopResponse)
	}
	if c.MaxArtifactBytes != nil && *c.MaxArtifactBytes < 0 {
		return errors.New("max_artifact_bytes must not be negative")
	}
	if l := c.Limits; l != nil && (l.Default.Negative() || l.Max.Negative()) {
		return errors.New("limits must not be negative")
	}
	if p := c.Concurrency; p != nil && (min(p.PerTenant, p.PerWorkflow, p.PerAgent, p.PerTool) < 0 ||
		p.MaxActiveRuns != nil && *p.MaxActiveRuns < 0 || p.MaxQueuedRuns != nil && *p.MaxQueuedRuns < 0) {
		return errors.New("concurrency must not be negative")
	}
	for name, d := range map[string]*time.Duration{"approval_timeout": c.ApprovalTimeout, "idle_warn_after": c.IdleWarnAfter} {
		if d != nil && *d < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if c.MaxHistoryMessages != nil && *c.MaxHistoryMessages < 0 {
		return errors.New("max_history_messages must not be negative")
	}
	if r := c.ToolRetry; r != nil && (r.MaxAttempts < 0 || r.Backoff < 0) {
		return errors.New("tool_retry must not be negative")
	}
	if r := c.Retention; r != nil && (r.Sessions < 0 || r.Memories < 0) {
		return errors.New("retention must not be negative")
	}
	names := map[string]bool{}
	for _, w := range c.Workflows {
		if w.Name == "" || names[w.Name] {
			return fmt.Errorf("workflow name %q is empty or used twice", w.Name)
		}
		names[w.Name] = true
		if _, err := compileWorkflow(w, agents); err != nil {
			return fmt.Errorf("workflow %q: %w", w.Name, err)
		}
	}
	return nil
}
