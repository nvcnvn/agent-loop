package runtime

import (
	"sync"

	"github.com/nvcnvn/agent-loop/core"
)

// Delta is part of an agent's reply as the model writes it. Deltas are not
// stored: a reply that ends in tool calls or fails its output schema is
// superseded by the next model call (a new StepID), and the finished reply
// is the agent_message_created event.
type Delta struct {
	RunID   core.RunID  `json:"run_id"`
	TaskID  core.TaskID `json:"task_id"`
	StepID  core.StepID `json:"step_id"`
	Agent   string      `json:"agent"`
	Content string      `json:"content"`
}

type deltaHub struct {
	mu   sync.Mutex
	subs map[core.RunID]map[chan Delta]struct{}
}

// SubscribeDeltas streams the reply deltas of a run's model calls until
// cancel is called. A subscriber that falls 256 deltas behind loses the
// excess; the stored events stay complete.
func (o *Orchestrator) SubscribeDeltas(run core.RunID) (<-chan Delta, func()) {
	h := &o.deltas
	ch := make(chan Delta, 256)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[core.RunID]map[chan Delta]struct{}{}
	}
	if h.subs[run] == nil {
		h.subs[run] = map[chan Delta]struct{}{}
	}
	h.subs[run][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs[run], ch)
		if len(h.subs[run]) == 0 {
			delete(h.subs, run)
		}
	}
}

func (h *deltaHub) publish(d Delta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[d.RunID] {
		select {
		case ch <- d:
		default:
		}
	}
}
