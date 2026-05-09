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

// CheckToolCall returns ErrBudgetExceeded if scheduling another tool call
// would exceed limits.
func (b *BudgetManager) CheckToolCall(run core.RunID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.usage[run]
	l := b.limits[run]
	if u != nil && l.MaxToolCalls > 0 && u.ToolCalls >= int64(l.MaxToolCalls) {
		return fmt.Errorf("%w: tool calls", core.ErrBudgetExceeded)
	}
	return nil
}

// CheckModelCall returns ErrBudgetExceeded if scheduling another model call
// would exceed limits.
func (b *BudgetManager) CheckModelCall(run core.RunID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.usage[run]
	l := b.limits[run]
	if u != nil && l.MaxModelCalls > 0 && u.ModelCalls >= int64(l.MaxModelCalls) {
		return fmt.Errorf("%w: model calls", core.ErrBudgetExceeded)
	}
	return nil
}

// CheckSubAgent returns ErrBudgetExceeded if spawning another sub-agent
// would exceed limits.
func (b *BudgetManager) CheckSubAgent(run core.RunID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.usage[run]
	l := b.limits[run]
	if u != nil && l.MaxSubAgents > 0 && u.SubAgents >= int64(l.MaxSubAgents) {
		return fmt.Errorf("%w: sub agents", core.ErrBudgetExceeded)
	}
	return nil
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
	l := b.limits[run]
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

// Snapshot returns the current accumulated usage.
func (b *BudgetManager) Snapshot(run core.RunID) core.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u := b.usage[run]; u != nil {
		return *u
	}
	return core.Usage{}
}
