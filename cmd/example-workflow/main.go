// Command example-workflow demonstrates programmatic use of the agent-loop
// runtime: a sequential workflow, a parallel sub-agent fan-out, and a
// strict replay verification — all without an external service.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s := store.NewMemStore()
	reg := tool.NewRegistry()
	if err := tool.RegisterBuiltins(reg); err != nil {
		log.Fatal(err)
	}

	// Two scripted agents: a planner that emits a plan and three workers
	// that emit one section each.
	planner := model.NewMockProvider(model.MockResponse{Content: "Plan: research, draft, review."})
	researcher := model.NewMockProvider(model.MockResponse{Content: "research-output"})
	drafter := model.NewMockProvider(model.MockResponse{Content: "draft-output"})
	reviewer := model.NewMockProvider(model.MockResponse{Content: "review-output"})
	synth := model.NewMockProvider(model.MockResponse{Content: "final synthesis combining all three"})

	agents := []runtime.Agent{
		{Name: "planner", Model: "mock", Provider: planner, Instructions: "Produce a plan."},
		{Name: "researcher", Model: "mock", Provider: researcher},
		{Name: "drafter", Model: "mock", Provider: drafter},
		{Name: "reviewer", Model: "mock", Provider: reviewer},
		{Name: "synthesizer", Model: "mock", Provider: synth},
	}
	orch := runtime.NewOrchestrator(s, reg, agents)

	sid, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal, Title: "demo"})
	if err != nil {
		log.Fatal(err)
	}

	wf := runtime.Workflow{
		Name: "plan-research-synthesize",
		Stages: []runtime.Stage{
			{
				Name: "plan",
				Kind: runtime.StageSequential,
				Agents: []runtime.AgentStep{
					{Name: "planner", Prompt: "Plan a report about {{input.topic}}."},
				},
			},
			{
				Name:          "research",
				Kind:          runtime.StageParallel,
				FailurePolicy: runtime.WaitAll,
				Agents: []runtime.AgentStep{
					{Name: "researcher", Prompt: "Research {{input.topic}} (plan: {{stage.plan.planner}})."},
					{Name: "drafter", Prompt: "Draft an outline."},
					{Name: "reviewer", Prompt: "Review the plan."},
				},
			},
			{
				Name: "synthesize",
				Kind: runtime.StageSequential,
				Agents: []runtime.AgentStep{
					{Name: "synthesizer", Prompt: "Synthesize: {{stage.research.researcher}} | {{stage.research.drafter}} | {{stage.research.reviewer}}."},
				},
			},
		},
	}

	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		TenantID:    core.DefaultTenant,
		PrincipalID: core.DefaultPrincipal,
		SessionID:   sid,
		Workflow:    wf,
		Input:       map[string]string{"topic": "agent runtimes"},
	})
	if err != nil {
		log.Fatal(err)
	}

	res, err := orch.Wait(ctx, runID)
	if err != nil {
		log.Fatalf("run failed: %v (status=%s reason=%s)", err, res.Status, res.FailureReason)
	}
	fmt.Printf("Run %s -> %s\n", runID, res.Status)
	for k, v := range res.Outputs {
		fmt.Printf("  stage %s: %s\n", k, v)
	}
	fmt.Printf("Usage: %+v\n", res.Usage)

	// Strict replay: rebuild terminal state from events and verify it matches.
	replayed, err := runtime.Replay(ctx, s, core.DefaultTenant, runID)
	if err != nil {
		log.Fatalf("replay failed: %v", err)
	}
	fmt.Printf("Replay terminal status: %s\n", replayed.Status)
}
