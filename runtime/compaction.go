package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/store"
)

// Compaction summarizes older session history when a run starts, so agents
// see the latest summary and the recent messages instead of everything, and
// the older rounds of a task's tool loop when they grow too long. The raw
// messages and model calls stay stored; a history summary is an internal
// session message.
type Compaction struct {
	// Agent writes the summary with its provider, model and instructions;
	// empty disables compaction.
	Agent string `json:"agent"`
	// MaxMessages and MaxTokens trigger a summary when the messages since the
	// last one exceed either; zero ignores that trigger. Tokens are estimated
	// as characters / 4.
	MaxMessages int   `json:"max_messages,omitempty"`
	MaxTokens   int64 `json:"max_tokens,omitempty"`
	// KeepRecent messages stay verbatim after the summary.
	KeepRecent int `json:"keep_recent,omitempty"`
	// TaskMaxTokens triggers, once a task's messages since its prompt pass this
	// many estimated tokens, a summary of its tool-loop rounds before the
	// latest; zero disables it.
	TaskMaxTokens int64 `json:"task_max_tokens,omitempty"`
}

// snapshotRole marks the session message that holds a history summary.
const snapshotRole = "snapshot"

// snapshot is the content of a snapshotRole message.
type snapshot struct {
	Summary string `json:"summary"`
	// ThroughID is the last summarized message; the messages after it are
	// not in the summary. Through is when it was written, which snapshots
	// without ThroughID are cut at.
	ThroughID string    `json:"through_id,omitempty"`
	Through   time.Time `json:"through"`
	// Messages counts every message the summary covers.
	Messages int `json:"messages"`
}

const summaryPrompt = "Write an updated summary of the whole conversation for the assistant that continues it. " +
	"Keep every fact, name, number, decision, preference and open question a later reply may need. " +
	"Reply with only the summary."

// sessionHistory returns what agents of the run see of its session: the
// latest summary, then the messages written after it by humans and earlier
// runs, at most MaxHistoryMessages of them. It first compacts the history
// when Compaction says so.
func (o *Orchestrator) sessionHistory(ctx context.Context, in StartRunInput, runID core.RunID) ([]model.Message, error) {
	stored, err := o.Store.ListMessages(ctx, in.TenantID, in.SessionID)
	if err != nil {
		return nil, err
	}
	var last snapshot
	var tail []store.MessageProjection
	for _, m := range stored {
		if m.Role == snapshotRole {
			var s snapshot
			if json.Unmarshal([]byte(m.Content), &s) == nil {
				last = s
			}
			continue
		}
		if m.RunID != runID {
			tail = append(tail, m)
		}
	}
	if i := slices.IndexFunc(tail, func(m store.MessageProjection) bool { return m.ID == last.ThroughID }); last.ThroughID != "" && i >= 0 {
		tail = tail[i+1:]
	} else {
		tail = slices.DeleteFunc(tail, func(m store.MessageProjection) bool { return !m.CreatedAt.After(last.Through) })
	}

	if c := o.Compaction; c.Agent != "" && c.exceeded(tail) {
		keep := min(max(c.KeepRecent, 0), len(tail))
		older := tail[:len(tail)-keep]
		if len(older) > 0 {
			if last, err = o.compact(ctx, in, runID, last, older); err != nil {
				return nil, fmt.Errorf("compact history: %w", err)
			}
			tail = tail[len(older):]
		}
	}

	if o.MaxHistoryMessages > 0 && len(tail) > o.MaxHistoryMessages {
		tail = tail[len(tail)-o.MaxHistoryMessages:]
	}
	var out []model.Message
	if last.Summary != "" {
		out = append(out, model.Message{Role: model.RoleUser, Content: "Summary of the earlier conversation:\n" + last.Summary})
	}
	for _, m := range tail {
		out = append(out, historyMessage(m))
	}
	return out, nil
}

// historyMessage maps a stored message to a model turn. Only messages a run
// wrote are assistant turns, whatever role a client posted with.
func historyMessage(m store.MessageProjection) model.Message {
	role := model.RoleUser
	if m.RunID != "" {
		role = model.RoleAssistant
	}
	return model.Message{Role: role, Content: m.Content}
}

func (c Compaction) exceeded(msgs []store.MessageProjection) bool {
	if c.MaxMessages > 0 && len(msgs) > c.MaxMessages {
		return true
	}
	var chars int64
	for _, m := range msgs {
		chars += int64(len(m.Content))
	}
	// ponytail: characters/4 approximates tokens; use a tokenizer if budgets get tight.
	return c.MaxTokens > 0 && chars/4 > c.MaxTokens
}

// compact asks the Compaction agent to fold older into prev, stores the new
// summary as a snapshot message and records a snapshot_created event.
func (o *Orchestrator) compact(ctx context.Context, in StartRunInput, runID core.RunID, prev snapshot, older []store.MessageProjection) (snapshot, error) {
	agent, ok := o.Agents[o.Compaction.Agent]
	if !ok {
		return prev, fmt.Errorf("compaction agent %q: %w", o.Compaction.Agent, core.ErrNotFound)
	}
	var b strings.Builder
	if prev.Summary != "" {
		fmt.Fprintf(&b, "Summary so far:\n%s\n\n", prev.Summary)
	}
	b.WriteString("Conversation since then:\n")
	for _, m := range older {
		fmt.Fprintf(&b, "[%s] %s\n", historyMessage(m).Role, m.Content)
	}
	b.WriteString("\n" + summaryPrompt)
	var msgs []model.Message
	if agent.Instructions != "" {
		msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: agent.Instructions})
	}
	msgs = append(msgs, model.Message{Role: model.RoleUser, Content: b.String()})
	resp, err := o.modelCall(ctx, runID, in, "", agent, model.ChatRequest{Messages: msgs})
	if err != nil {
		return prev, err
	}
	next := snapshot{
		Summary:   strings.TrimSpace(resp.Message.Content),
		ThroughID: older[len(older)-1].ID,
		Through:   older[len(older)-1].CreatedAt,
		Messages:  prev.Messages + len(older),
	}
	if next.Summary == "" {
		return prev, fmt.Errorf("agent %s wrote an empty summary", agent.Name)
	}
	if err := o.Store.AddMessage(ctx, store.MessageProjection{
		SessionID:  in.SessionID,
		RunID:      runID,
		Role:       snapshotRole,
		Visibility: core.VisibilityInternal,
		Content:    string(mustJSON(next)),
	}); err != nil {
		return prev, err
	}
	o.emit(ctx, core.Event{
		TenantID:      in.TenantID,
		SessionID:     in.SessionID,
		RunID:         runID,
		Type:          core.EvSnapshot,
		Visibility:    core.VisibilityInternal,
		ActorID:       core.AgentPrincipal(agent.Name),
		Payload:       mustJSON(map[string]any{"agent": agent.Name, "summarized": len(older), "covers": next.Messages, "through": next.Through}),
		HiddenPayload: mustJSON(map[string]any{"summary": next.Summary}),
	})
	return next, nil
}

const taskSummaryPrompt = "Write a summary of the work on this task so far for the assistant that continues it: " +
	"what was tried, every tool result, fact, name, number and decision it still needs, and what is left to do. " +
	"Reply with only the summary."

// compactTask replaces the tool-loop rounds of a task before its latest with a
// summary once the messages since its prompt, from messages[base], pass
// TaskMaxTokens. A summary of earlier rounds is folded into the next one.
func (o *Orchestrator) compactTask(ctx context.Context, in StartRunInput, runID core.RunID, taskID core.TaskID, prompt string, messages []model.Message, base int) ([]model.Message, error) {
	c := o.Compaction
	if c.Agent == "" || c.TaskMaxTokens <= 0 || estimateTokens(messages[base:]) <= c.TaskMaxTokens {
		return messages, nil
	}
	cut := base
	for i := len(messages) - 1; i > base; i-- {
		if messages[i].Role == model.RoleAssistant && len(messages[i].ToolCalls) > 0 {
			cut = i
			break
		}
	}
	if !slices.ContainsFunc(messages[base:cut], func(m model.Message) bool { return m.Role == model.RoleAssistant }) {
		return messages, nil
	}
	agent, ok := o.Agents[c.Agent]
	if !ok {
		return nil, fmt.Errorf("compaction agent %q: %w", c.Agent, core.ErrNotFound)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task:\n%s\n\nWork so far:\n", prompt)
	for _, m := range messages[base:cut] {
		writeTurn(&b, m)
	}
	b.WriteString("\n" + taskSummaryPrompt)
	var msgs []model.Message
	if agent.Instructions != "" {
		msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: agent.Instructions})
	}
	msgs = append(msgs, model.Message{Role: model.RoleUser, Content: b.String()})
	resp, err := o.modelCall(ctx, runID, in, taskID, agent, model.ChatRequest{Messages: msgs})
	if err != nil {
		return nil, err
	}
	summary := strings.TrimSpace(resp.Message.Content)
	if summary == "" {
		return nil, fmt.Errorf("agent %s wrote an empty task summary", agent.Name)
	}
	o.emit(ctx, core.Event{
		TenantID:      in.TenantID,
		SessionID:     in.SessionID,
		RunID:         runID,
		TaskID:        taskID,
		Type:          core.EvSnapshot,
		Visibility:    core.VisibilityInternal,
		ActorID:       core.AgentPrincipal(agent.Name),
		Payload:       mustJSON(map[string]any{"scope": "task", "agent": agent.Name, "summarized": cut - base}),
		HiddenPayload: mustJSON(map[string]any{"summary": summary}),
	})
	out := slices.Clone(messages[:base])
	out = append(out, model.Message{Role: model.RoleUser, Content: "Summary of your work on this task so far:\n" + summary})
	return append(out, messages[cut:]...), nil
}

// writeTurn writes a tool-loop message as a line of a summary prompt.
func writeTurn(b *strings.Builder, m model.Message) {
	switch {
	case m.Role == model.RoleTool:
		fmt.Fprintf(b, "[tool %s returned] %s\n", m.Name, m.Content)
	case len(m.ToolCalls) > 0:
		if m.Content != "" {
			fmt.Fprintf(b, "[%s] %s\n", m.Role, m.Content)
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(b, "[assistant called %s] %s\n", tc.Name, tc.Input)
		}
	default:
		fmt.Fprintf(b, "[%s] %s\n", m.Role, m.Content)
	}
}

// estimateTokens estimates tokens as characters / 4.
func estimateTokens(msgs []model.Message) int64 {
	var chars int
	for _, m := range msgs {
		chars += len(m.Content) + len(m.Thinking)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(tc.Input)
		}
	}
	return int64(chars / 4)
}
