package scenario_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

var recordRuns = flag.String("record-runs", "", "save the runs of the scenarios of this model slug (e.g. z-ai_glm-5.3) as run fixtures in testdata/runs")

// runFixture is a finished run as a store holds it, with the agents, tools
// and settings of the orchestrator that ran it.
type runFixture struct {
	Agents     []runtime.Agent
	Tools      []core.ToolDefinition
	Compaction runtime.Compaction
	Run        store.Run
	History    []store.MessageProjection
	Inputs     []inputImage
	Events     []core.Event
}

// inputImage is an input artifact with its content, which store.Artifact leaves out of JSON.
type inputImage struct {
	store.Artifact
	Content []byte
}

func (w *world) saveRun(runID core.RunID) {
	w.t.Helper()
	ctx := context.Background()
	run, err := w.store.GetRun(ctx, core.DefaultTenant, runID)
	if err != nil {
		w.t.Fatal(err)
	}
	f := runFixture{Tools: w.reg.List(), Compaction: w.orch.Compaction, Run: run}
	for _, name := range slices.Sorted(maps.Keys(w.orch.Agents)) {
		f.Agents = append(f.Agents, w.orch.Agents[name])
	}
	msgs, err := w.store.ListMessages(ctx, core.DefaultTenant, w.sid)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, m := range msgs {
		if m.RunID != runID && m.CreatedAt.Before(run.StartedAt) {
			f.History = append(f.History, m)
		}
	}
	artifacts, err := w.store.ListRunArtifacts(ctx, core.DefaultTenant, runID)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, a := range artifacts {
		if a.Producer == "" {
			f.Inputs = append(f.Inputs, inputImage{a, a.Content})
		}
	}
	if f.Events, err = w.store.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0); err != nil {
		w.t.Fatal(err)
	}
	scenarioName, modelSlug, _ := strings.Cut(w.t.Name(), "/")
	dir := filepath.Join("testdata", "runs", scenarioName)
	if w.saved == 0 {
		stale, _ := filepath.Glob(filepath.Join(dir, modelSlug+"-*.json"))
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}
	w.saved++
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.json", modelSlug, w.saved))
	data, err := json.Marshal(f)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	if err == nil {
		err = os.WriteFile(path, append(data, '\n'), 0o644)
	}
	if err != nil {
		w.t.Fatal(err)
	}
}

// Acceptance: §Persistence and Replay #2b – runs recorded by earlier versions re-execute to the same end on this one.
func TestRecordedRunsReExecute(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "runs", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no run fixtures; record them with go test ./scenario -args -record-runs=z-ai_glm-5.3")
	}
	for _, path := range paths {
		t.Run(strings.TrimSuffix(strings.TrimPrefix(path, filepath.Join("testdata", "runs")+string(filepath.Separator)), ".json"), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var f runFixture
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatal(err)
			}
			orch, err := loadRun(f)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			again, err := orch.ReExecute(ctx, f.Run.TenantID, f.Run.ID, core.SystemPrincipal)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			if len(f.Run.Output) > 0 {
				if err := json.Unmarshal(f.Run.Output, &want); err != nil {
					t.Fatal(err)
				}
			}
			if again.Status != f.Run.Status || !maps.Equal(again.Outputs, want) {
				t.Fatalf("re-execution ended %s (%s) with %v; recorded %s with %v", again.Status, again.FailureReason, again.Outputs, f.Run.Status, want)
			}
		})
	}
}

// loadRun puts a recorded run into a fresh store under an orchestrator with
// the recorded agents and tools, whose executors are never called on re-execution.
func loadRun(f runFixture) (*runtime.Orchestrator, error) {
	ctx := context.Background()
	s := store.NewMemStore()
	if _, err := s.CreateSession(ctx, store.Session{ID: f.Run.SessionID, TenantID: f.Run.TenantID, CreatedBy: core.DefaultPrincipal}); err != nil {
		return nil, err
	}
	for _, m := range f.History {
		if err := s.AddMessage(ctx, m); err != nil {
			return nil, err
		}
	}
	if _, err := s.CreateRun(ctx, f.Run); err != nil {
		return nil, err
	}
	for _, in := range f.Inputs {
		in.Artifact.Content = in.Content
		if _, err := s.PutArtifact(ctx, in.Artifact); err != nil {
			return nil, err
		}
	}
	if _, err := s.Append(ctx, f.Events); err != nil {
		return nil, err
	}
	reg := tool.NewRegistry()
	unused := tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{}, errors.New("re-execution called a live tool")
	})
	for _, def := range f.Tools {
		if err := reg.Register(def, unused); err != nil {
			return nil, err
		}
	}
	orch := runtime.NewOrchestrator(s, reg, f.Agents)
	orch.Compaction = f.Compaction
	return orch, nil
}
