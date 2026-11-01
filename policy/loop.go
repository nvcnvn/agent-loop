package policy

import (
	"strings"
	"sync"
	"unicode"

	"github.com/nvcnvn/agent-loop/core"
)

// LoopDetector tracks repeated tool calls and near-identical model replies
// per run and flags stall conditions per requirement §9.1.
type LoopDetector struct {
	mu        sync.Mutex
	threshold int
	counts    map[core.RunID]map[string]int
	// ReplyRepeats is how many near-identical replies in one task are a stall; at most one less are kept.
	ReplyRepeats int
	// ReplySimilarity is the word-set Jaccard similarity from which two replies are near-identical.
	ReplySimilarity float64
	replies         map[core.RunID]map[core.TaskID][]map[string]bool
}

// NewLoopDetector flags threshold identical tool calls in a run (default 5)
// and three near-identical replies in a task.
func NewLoopDetector(threshold int) *LoopDetector {
	if threshold <= 0 {
		threshold = 5
	}
	return &LoopDetector{
		threshold: threshold, counts: map[core.RunID]map[string]int{},
		ReplyRepeats: 3, ReplySimilarity: 0.9, replies: map[core.RunID]map[core.TaskID][]map[string]bool{},
	}
}

// ObserveReply records a reply of a task that the runtime sent back to the
// model, and returns true when it and the task's last ReplyRepeats-1 such
// replies are all near-identical. An empty reply is ignored.
func (d *LoopDetector) ObserveReply(run core.RunID, task core.TaskID, reply string) (stalled bool, count int) {
	words := wordSet(reply)
	if len(words) == 0 {
		return false, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tasks := d.replies[run]
	if tasks == nil {
		tasks = map[core.TaskID][]map[string]bool{}
		d.replies[run] = tasks
	}
	count = 1
	for _, earlier := range tasks[task] {
		if jaccard(words, earlier) >= d.ReplySimilarity {
			count++
		}
	}
	kept := append(tasks[task], words)
	tasks[task] = kept[max(0, len(kept)-(d.ReplyRepeats-1)):]
	return d.ReplyRepeats > 0 && count >= d.ReplyRepeats, count
}

func wordSet(s string) map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		words[w] = true
	}
	return words
}

func jaccard(a, b map[string]bool) float64 {
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	return float64(shared) / float64(len(a)+len(b)-shared)
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
	delete(d.replies, run)
}
