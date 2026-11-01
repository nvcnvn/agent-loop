package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

// flakyLeases is a store whose lease renewals fail while broken is set.
type flakyLeases struct {
	store.Store
	broken       atomic.Bool
	releaseDelay time.Duration
}

func (f *flakyLeases) ReleaseRun(ctx context.Context, id core.RunID, owner string) error {
	time.Sleep(f.releaseDelay)
	return f.Store.ReleaseRun(ctx, id, owner)
}

func (f *flakyLeases) RenewLeases(ctx context.Context, owner string, runs []core.RunID, lease time.Duration) (map[core.RunID]bool, error) {
	if f.broken.Load() {
		return nil, errors.New("database unreachable")
	}
	return f.Store.RenewLeases(ctx, owner, runs, lease)
}

// process is one of several orchestrators sharing a store, as API replicas do.
func process(t *testing.T, s store.Store, owner string, prov model.Provider) *runtime.Orchestrator {
	t.Helper()
	reg := tool.NewRegistry()
	o := runtime.NewOrchestrator(s, reg, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov}})
	o.Owner, o.Lease, o.IdleWarnAfter = owner, 60*time.Millisecond, 0
	return o
}

// Acceptance: §Observability and Operations #6 – processes sharing a store run each run once, hand runs over and cancel each other's.
func TestRunsLeasedAcrossProcesses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shared := &flakyLeases{Store: store.NewMemStore()}
	sid, _ := shared.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	slow := &slowProvider{delay: time.Hour}
	a, b := process(t, shared, "a", slow), process(t, shared, "b", slow)

	t.Run("cancel elsewhere", func(t *testing.T) {
		runID, err := a.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
		if err != nil {
			t.Fatal(err)
		}
		if n, err := b.Recover(ctx); n != 0 || err != nil {
			t.Fatalf("b took over a live run: %d %v", n, err)
		}
		if err := b.Resume(ctx, core.DefaultTenant, runID, core.DefaultPrincipal); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("b resumed a's run: %v", err)
		}
		if err := b.Cancel(ctx, core.DefaultTenant, runID, core.DefaultPrincipal, "stop"); err != nil {
			t.Fatal(err)
		}
		if res, _ := a.Wait(ctx, core.DefaultTenant, runID); res.Status != core.RunCanceled {
			t.Fatalf("a's run after b canceled it: %+v", res)
		}
	})

	t.Run("hand over on shutdown", func(t *testing.T) {
		c := process(t, shared, "c", slow)
		runID, err := c.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		now, stop := context.WithTimeout(ctx, time.Millisecond)
		defer stop()
		_ = c.Shutdown(now)
		fast := process(t, shared, "d", model.NewMockProvider(model.MockResponse{Content: "finished by d"}))
		if n, err := fast.Recover(ctx); n != 1 || err != nil {
			t.Fatalf("d did not take over the released run: %d %v", n, err)
		}
		if res, err := fast.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Outputs["s"] != "finished by d" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("stop when the lease cannot be renewed", func(t *testing.T) {
		e := process(t, shared, "e", slow)
		runID, err := e.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
		if err != nil {
			t.Fatal(err)
		}
		shared.broken.Store(true)
		time.Sleep(200 * time.Millisecond)
		shared.broken.Store(false)
		if run, _ := shared.GetRun(ctx, core.DefaultTenant, runID); run.Status.Terminal() {
			t.Fatalf("a process that lost its lease wrote a final status: %s", run.Status)
		}
		fast := process(t, shared, "f", model.NewMockProvider(model.MockResponse{Content: "finished by f"}))
		if n, err := fast.Recover(ctx); n != 1 || err != nil {
			t.Fatalf("f did not take over: %d %v", n, err)
		}
		if res, err := fast.Wait(ctx, core.DefaultTenant, runID); err != nil || res.Outputs["s"] != "finished by f" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if e.ActiveRuns() != 0 {
			t.Fatal("the fenced process still executes the run")
		}
	})
}

// A finished run's lease is released before Wait returns, so another process can resume it at once.
func TestLeaseReleasedBeforeWaitReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shared := &flakyLeases{Store: store.NewMemStore(), releaseDelay: 50 * time.Millisecond}
	sid, _ := shared.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	a := process(t, shared, "a", model.NewMockProvider(model.MockResponse{Content: "done"}))
	runID, err := a.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}
	if run, _ := shared.GetRun(ctx, core.DefaultTenant, runID); run.Owner != "" {
		t.Fatalf("lease still held by %q after Wait returned", run.Owner)
	}
}

// Canceling a run in the process executing it also records the request, so
// the process that recovers the run if this one dies cancels it too.
func TestLocalCancelIsRecorded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shared := &flakyLeases{Store: store.NewMemStore()}
	sid, _ := shared.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	a := process(t, shared, "a", &slowProvider{delay: time.Hour})
	runID, err := a.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Cancel(ctx, core.DefaultTenant, runID, core.DefaultPrincipal, "stop"); err != nil {
		t.Fatal(err)
	}
	if run, _ := shared.GetRun(ctx, core.DefaultTenant, runID); !run.CancelRequested {
		t.Fatal("the cancel request was not stored")
	}
	_, _ = a.Wait(ctx, core.DefaultTenant, runID)
}

// A run canceled while no process executed it ends with its tasks and
// approvals closed, not left running and pending.
func TestRecoverCancelsRequestedRunCleanly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := store.NewMemStore()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	wf, _ := json.Marshal(oneStage("ag"))
	runID, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning, Workflow: wf})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, store.Task{TenantID: core.DefaultTenant, RunID: runID, AgentName: "ag", Status: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	approval, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestCancel(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}
	b := process(t, s, "b", model.NewMockProvider())
	if _, err := b.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if run, _ := s.GetRun(ctx, core.DefaultTenant, runID); run.Status != core.RunCanceled {
		t.Fatalf("run %s", run.Status)
	}
	tasks, _ := s.ListRunTasks(ctx, core.DefaultTenant, runID)
	if len(tasks) != 1 || tasks[0].Status == core.TaskRunning {
		t.Fatalf("tasks %+v", tasks)
	}
	if a, _ := s.GetApproval(ctx, core.DefaultTenant, approval); a.Status == "requested" {
		t.Fatal("approval left pending")
	}
}
