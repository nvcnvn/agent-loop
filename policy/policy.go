// Package policy provides budget, approval, tool-policy and loop-detection
// services used by the runtime to enforce safety constraints from §9 of the
// requirements.
package policy

import (
	"fmt"
	"sync"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/tool"
)

// ToolPolicy is the per-run policy applied to tool calls.
type ToolPolicy struct {
	Allowlist        []string // empty == allow all
	Denylist         []string
	ApprovalRequired []string // tool names requiring approval
}

func (p ToolPolicy) Authorize(t tool.Definition) (allowed bool, requiresApproval bool, reason string) {
	for _, d := range p.Denylist {
		if d == t.Name {
			return false, false, "tool in denylist"
		}
	}
	if len(p.Allowlist) > 0 {
		ok := false
		for _, a := range p.Allowlist {
			if a == t.Name {
				ok = true
				break
			}
		}
		if !ok {
			return false, false, "tool not in allowlist"
		}
	}
	if t.RequiresApproval {
		return true, true, "tool definition requires approval"
	}
	for _, n := range p.ApprovalRequired {
		if n == t.Name {
			return true, true, "tool requires approval per policy"
		}
	}
	return true, false, ""
}

// BudgetManager tracks per-run usage against limits with reserve/commit/release.
type BudgetManager struct {
	mu     sync.Mutex
	usage  map[core.RunID]*core.Usage
	limits map[core.RunID]core.Limits
}

func NewBudgetManager() *BudgetManager {
	return &BudgetManager{
		usage:  map[core.RunID]*core.Usage{},
		limits: map[core.RunID]core.Limits{},
	}
}

// Init declares the limits for a run. Idempotent.
func (b *BudgetManager) Init(run core.RunID, limits core.Limits) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.usage[run]; !ok {
		b.usage[run] = &core.Usage{}
	}
	b.limits[run] = limits.Defaults()
}

// ReserveToolCall reserves capacity before scheduling a tool call.
func (b *BudgetManager) ReserveToolCall(run core.RunID) error {
	return b.Reserve(run, core.Usage{ToolCalls: 1})
}

// ReserveToolCalls reserves capacity before scheduling a tool-call group.
func (b *BudgetManager) ReserveToolCalls(run core.RunID, count int) error {
	if count <= 0 {
		return nil
	}
	return b.Reserve(run, core.Usage{ToolCalls: int64(count)})
}

// ReserveModelCall reserves capacity before invoking a model provider.
func (b *BudgetManager) ReserveModelCall(run core.RunID) error {
	return b.Reserve(run, core.Usage{ModelCalls: 1})
}

// ReserveSubAgent reserves capacity before spawning a sub-agent task.
func (b *BudgetManager) ReserveSubAgent(run core.RunID) error {
	return b.Reserve(run, core.Usage{SubAgents: 1})
}

// Reserve records usage before execution starts. If the reservation would
// exceed a limit, usage is left unchanged.
func (b *BudgetManager) Reserve(run core.RunID, used core.Usage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.usage[run]
	if u == nil {
		u = &core.Usage{}
		b.usage[run] = u
	}
	next := *u
	next.Add(used)
	if err := checkLimits(next, b.limits[run]); err != nil {
		return err
	}
	*u = next
	return nil
}

// Release rolls back a reservation that did not start execution.
func (b *BudgetManager) Release(run core.RunID, used core.Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.usage[run]
	if u == nil {
		return
	}
	u.PromptTokens = maxInt64(0, u.PromptTokens-used.PromptTokens)
	u.CompletionTokens = maxInt64(0, u.CompletionTokens-used.CompletionTokens)
	u.CachedTokens = maxInt64(0, u.CachedTokens-used.CachedTokens)
	u.TotalTokens = maxInt64(0, u.TotalTokens-used.TotalTokens)
	u.CostUnits = maxInt64(0, u.CostUnits-used.CostUnits)
	u.ModelCalls = maxInt64(0, u.ModelCalls-used.ModelCalls)
	u.ToolCalls = maxInt64(0, u.ToolCalls-used.ToolCalls)
	u.SubAgents = maxInt64(0, u.SubAgents-used.SubAgents)
	u.Runtime -= used.Runtime
	if u.Runtime < 0 {
		u.Runtime = 0
	}
}

// Commit records actual usage after a successful call.
func (b *BudgetManager) Commit(run core.RunID, used core.Usage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.usage[run]
	if u == nil {
		u = &core.Usage{}
		b.usage[run] = u
	}
	u.Add(used)
	return checkLimits(*u, b.limits[run])
}

// Snapshot returns the current accumulated usage.
func (b *BudgetManager) Snapshot(run core.RunID) core.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u := b.usage[run]; u != nil {
		return *u
	}
	return core.Usage{}
}

func checkLimits(u core.Usage, l core.Limits) error {
	if l.MaxToolCalls > 0 && u.ToolCalls > int64(l.MaxToolCalls) {
		return fmt.Errorf("%w: tool calls", core.ErrBudgetExceeded)
	}
	if l.MaxModelCalls > 0 && u.ModelCalls > int64(l.MaxModelCalls) {
		return fmt.Errorf("%w: model calls", core.ErrBudgetExceeded)
	}
	if l.MaxSubAgents > 0 && u.SubAgents > int64(l.MaxSubAgents) {
		return fmt.Errorf("%w: sub agents", core.ErrBudgetExceeded)
	}
	if l.MaxPromptTokens > 0 && u.PromptTokens > l.MaxPromptTokens {
		return fmt.Errorf("%w: prompt tokens", core.ErrBudgetExceeded)
	}
	if l.MaxCompletionTokens > 0 && u.CompletionTokens > l.MaxCompletionTokens {
		return fmt.Errorf("%w: completion tokens", core.ErrBudgetExceeded)
	}
	if l.MaxCostUnits > 0 && u.CostUnits > l.MaxCostUnits {
		return fmt.Errorf("%w: cost units", core.ErrBudgetExceeded)
	}
	return nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
