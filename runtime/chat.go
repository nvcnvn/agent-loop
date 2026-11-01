package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

// askHumanTool is the built-in tool of agents with AskHuman.
const askHumanTool = "ask_human"

var askHumanSpec = model.ToolSpec{
	Name:        askHumanTool,
	Description: "Ask the people in this conversation a question and wait for their reply. Use it only when you cannot go on without their answer.",
	InputSchema: mustJSON(map[string]any{
		"type":       "object",
		"properties": map[string]any{"question": map[string]any{"type": "string"}},
		"required":   []string{"question"},
	}),
}

// inputGate marks the approvals that wait for a human message.
const inputGate = "input"

// askHuman asks the session's people a question as an approval of gate
// input, which the next message posted to the session answers, and returns
// that message, or that nobody answered.
func (o *Orchestrator) askHuman(in StartRunInput, runID core.RunID, taskID core.TaskID) tool.Executor {
	return tool.ExecutorFunc(func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var req struct {
			Question string `json:"question"`
		}
		if err := json.Unmarshal(call.Input, &req); err != nil || strings.TrimSpace(req.Question) == "" {
			return tool.Result{}, errors.New(`ask_human takes {"question": "..."}`)
		}
		subject := map[string]any{"gate": inputGate, "question": req.Question}
		approval, err := o.awaitApproval(ctx, in.TenantID, in.SessionID, runID, taskID, call.StepID, "an agent asks a question", subject, mustJSON(subject))
		if err != nil {
			return tool.Result{}, err
		}
		var answer struct {
			Message string `json:"message"`
			Answer  string `json:"answer"`
		}
		_ = json.Unmarshal(approval.Resolution, &answer)
		if reply := cmp.Or(answer.Message, answer.Answer); approval.Status == "approved" && reply != "" {
			return tool.Result{Output: mustJSON(map[string]any{"answer": reply})}, nil
		}
		return tool.Result{Output: mustJSON(map[string]any{"answered": false, "status": approval.Status})}, nil
	})
}

// PostedMessage is what PostMessage did: the stored message, the run whose
// question it answered, or the run it started (Replayed when an earlier
// message with the run's idempotency key started it and nothing was posted).
type PostedMessage struct {
	Event    core.Event      `json:"event"`
	Answered core.RunID      `json:"answered_run_id,omitempty"`
	Approval core.ApprovalID `json:"answered_approval_id,omitempty"`
	RunID    core.RunID      `json:"run_id,omitempty"`
	Replayed bool            `json:"replayed,omitempty"`
}

// PostMessage stores a human message in a session, as an event and a
// message. Without run, it answers the oldest open ask_human question of the
// session's runs. With run, it checks the run first, then starts it in the
// session with the message as input "message"; an error after the message
// is stored comes back with the PostedMessage holding it.
func (o *Orchestrator) PostMessage(ctx context.Context, tenant core.TenantID, session core.SessionID, by core.PrincipalID, role, content string, run *StartRunInput) (PostedMessage, error) {
	if run != nil {
		run.TenantID, run.SessionID, run.PrincipalID = tenant, session, by
		if _, taken := run.Input["message"]; taken {
			return PostedMessage{}, errors.New(`input "message" is the posted message; leave it out`)
		}
		run.Input = maps.Clone(run.Input)
		if run.Input == nil {
			run.Input = map[string]string{}
		}
		run.Input["message"] = content
		existing, err := o.prepareRun(ctx, run)
		if err != nil {
			return PostedMessage{}, err
		}
		if existing != "" {
			return PostedMessage{RunID: existing, Replayed: true}, nil
		}
	}
	out, err := o.Store.Append(ctx, []core.Event{{
		TenantID:   tenant,
		SessionID:  session,
		Type:       core.EvHumanMessage,
		Visibility: core.VisibilityParticipant,
		ActorID:    by,
		Payload:    mustJSON(map[string]any{"role": role, "content": content}),
	}})
	if err != nil {
		return PostedMessage{}, err
	}
	posted := PostedMessage{Event: out[0]}
	if err := o.Store.AddMessage(ctx, store.MessageProjection{
		SessionID: session, EventID: posted.Event.ID, Role: role, Visibility: core.VisibilityParticipant, Content: content,
	}); err != nil {
		return posted, err
	}
	if run != nil {
		posted.RunID, err = o.createRun(ctx, *run)
		return posted, err
	}
	resolution := mustJSON(map[string]any{"message": content, "event_id": posted.Event.ID})
	for {
		question, err := o.openQuestion(ctx, tenant, session)
		if err != nil || question.ID == "" {
			return posted, err
		}
		err = o.Approvals.ResolveApproval(ctx, tenant, question.ID, "approved", by, resolution)
		if errors.Is(err, core.ErrConflict) {
			continue
		}
		if err != nil {
			return posted, err
		}
		posted.Answered, posted.Approval = question.RunID, question.ID
		return posted, nil
	}
}

// openQuestion returns the oldest open ask_human approval of the session's unfinished runs.
func (o *Orchestrator) openQuestion(ctx context.Context, tenant core.TenantID, session core.SessionID) (store.Approval, error) {
	runs, err := o.Store.ListRuns(ctx, tenant, session)
	if err != nil {
		return store.Approval{}, err
	}
	var open []store.Approval
	for _, run := range runs {
		if run.Status.Terminal() {
			continue
		}
		approvals, err := o.Approvals.ListApprovals(ctx, tenant, store.ApprovalFilter{RunID: run.ID, Status: "requested"})
		if err != nil {
			return store.Approval{}, err
		}
		for _, a := range approvals {
			var req struct {
				Gate string `json:"gate"`
			}
			if json.Unmarshal(a.Request, &req) == nil && req.Gate == inputGate {
				open = append(open, a)
			}
		}
	}
	if len(open) == 0 {
		return store.Approval{}, nil
	}
	return slices.MinFunc(open, func(a, b store.Approval) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	}), nil
}
