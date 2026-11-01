package policy

import (
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nvcnvn/agent-loop/core"
)

// LoopDetector tracks repeated tool calls, near-identical texts and retries
// per run and flags stall conditions per requirement §9.1.
type LoopDetector struct {
	mu        sync.Mutex
	threshold int
	counts    map[core.RunID]map[string]int
	// ReplyRepeats is how many near-identical texts in one scope are a stall; at most one less are kept.
	ReplyRepeats int
	// ReplySimilarity is the word-set Jaccard similarity from which two texts are near-identical.
	ReplySimilarity float64
	texts           map[core.RunID]map[string][]map[string]bool
	// RetryStorm is how many retries within RetryWindow are a retry storm; zero disables the check.
	RetryStorm  int
	RetryWindow time.Duration
	retries     map[core.RunID][]time.Time
}

// NewLoopDetector flags threshold identical tool calls in a run (default 5),
// three near-identical texts in a scope, and ten retries within a minute.
func NewLoopDetector(threshold int) *LoopDetector {
	if threshold <= 0 {
		threshold = 5
	}
	return &LoopDetector{
		threshold: threshold, counts: map[core.RunID]map[string]int{},
		ReplyRepeats: 3, ReplySimilarity: 0.9, texts: map[core.RunID]map[string][]map[string]bool{},
		RetryStorm: 10, RetryWindow: time.Minute, retries: map[core.RunID][]time.Time{},
	}
}

// ObserveReply records a text in a scope of a run, such as the answers a task
// gave that the runtime sent back, the drafts a stage handed back, or the tasks
// one agent delegated. It returns true when it and the scope's last
// ReplyRepeats-1 texts are all near-identical. An empty text is ignored.
func (d *LoopDetector) ObserveReply(run core.RunID, scope, text string) (stalled bool, count int) {
	words := wordSet(text)
	if len(words) == 0 {
		return false, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	scopes := d.texts[run]
	if scopes == nil {
		scopes = map[string][]map[string]bool{}
		d.texts[run] = scopes
	}
	count = 1
	for _, earlier := range scopes[scope] {
		if jaccard(words, earlier) >= d.ReplySimilarity {
			count++
		}
	}
	kept := append(scopes[scope], words)
	scopes[scope] = kept[max(0, len(kept)-(d.ReplyRepeats-1)):]
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
	key := ToolKey(toolName, inputHash)
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

// ToolKey is the key Observe counts a tool call under, for Clear.
func ToolKey(toolName, inputHash string) string { return toolName + "|" + inputHash }

// ObserveRetry records a retry in a run and returns true when RetryStorm
// retries, this one included, happened within RetryWindow.
func (d *LoopDetector) ObserveRetry(run core.RunID) (stalled bool, count int) {
	if d.RetryStorm <= 0 {
		return false, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	recent := d.retries[run][:0]
	for _, at := range d.retries[run] {
		if now.Sub(at) < d.RetryWindow {
			recent = append(recent, at)
		}
	}
	recent = append(recent, now)
	d.retries[run] = recent
	return len(recent) >= d.RetryStorm, len(recent)
}

// Clear forgets what a run observed under key, a tool key or a text scope,
// or its retries when key is empty, after a human let the run carry on.
func (d *LoopDetector) Clear(run core.RunID, key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if key == "" {
		delete(d.retries, run)
		return
	}
	delete(d.counts[run], key)
	delete(d.texts[run], key)
}

// Reset clears tracking for a run (e.g. when it ends).
func (d *LoopDetector) Reset(run core.RunID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.counts, run)
	delete(d.texts, run)
	delete(d.retries, run)
}
