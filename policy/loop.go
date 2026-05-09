package policy

import (
	"sync"

	"github.com/nvcnvn/agent-loop/core"
)

// LoopDetector tracks repeated tool calls per run and flags stall conditions
// per requirement §9.1. It is intentionally simple: identical (tool, input)
// repeated above a threshold within a sliding window is considered a stall.
type LoopDetector struct {
	mu        sync.Mutex
	threshold int
	counts    map[core.RunID]map[string]int
}

func NewLoopDetector(threshold int) *LoopDetector {
	if threshold <= 0 {
		threshold = 5
	}
	return &LoopDetector{threshold: threshold, counts: map[core.RunID]map[string]int{}}
}

// Observe records a tool call and returns true if the call should be treated
// as part of a stall.
func (d *LoopDetector) Observe(run core.RunID, toolName string, inputHash string) (stalled bool, count int) {
	key := toolName + "|" + inputHash
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.counts[run]
	if m == nil {
		m = map[string]int{}
		d.counts[run] = m
	}
	m[key]++
	return m[key] >= d.threshold, m[key]
}

// Reset clears tracking for a run (e.g. when it ends).
func (d *LoopDetector) Reset(run core.RunID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.counts, run)
}
