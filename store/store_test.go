package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/store/storetest"
)

func TestEventSequenceIsMonotonicUnderConcurrency(t *testing.T) {
	s := store.NewMemStore()
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	rid, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning})

	var wg sync.WaitGroup
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Append(ctx, []core.Event{{
				TenantID:   core.DefaultTenant,
				SessionID:  sid,
				RunID:      rid,
				Type:       core.EvSystem,
				Visibility: core.VisibilityInternal,
			}})
		}()
	}
	wg.Wait()

	events, err := s.ListRunEvents(ctx, core.DefaultTenant, rid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != N {
		t.Fatalf("got %d events, want %d", len(events), N)
	}
	seen := map[int64]bool{}
	for _, ev := range events {
		if ev.Sequence < 1 || ev.Sequence > int64(N) {
			t.Fatalf("sequence out of range: %d", ev.Sequence)
		}
		if seen[ev.Sequence] {
			t.Fatalf("duplicate sequence %d", ev.Sequence)
		}
		seen[ev.Sequence] = true
	}
}

func TestSessionTenantIsolation(t *testing.T) {
	s := store.NewMemStore()
	ctx := context.Background()
	t1 := core.TenantID("11111111-0000-0000-0000-000000000001")
	t2 := core.TenantID("22222222-0000-0000-0000-000000000001")
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: t1, CreatedBy: core.DefaultPrincipal})
	if _, err := s.GetSession(ctx, t2, sid); err == nil {
		t.Fatal("expected cross-tenant read to be denied")
	}
}

func TestSubscribeDeliversEventsAfterSeqAppendedBeforeSubscribing(t *testing.T) {
	s := store.NewMemStore()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.Append(ctx, []core.Event{{RunID: "r", Type: core.EvSystem}}); err != nil {
			t.Fatal(err)
		}
	}
	ch, cancel := s.Subscribe(ctx, "r", 1)
	defer cancel()
	if _, err := s.Append(ctx, []core.Event{{RunID: "r", Type: core.EvSystem}}); err != nil {
		t.Fatal(err)
	}
	for want := int64(2); want <= 4; want++ {
		if ev := <-ch; ev.Sequence != want {
			t.Fatalf("sequence=%d want %d", ev.Sequence, want)
		}
	}
}

func TestSubscribeDeliversInOrderUnderConcurrentAppendAndCancel(t *testing.T) {
	s := store.NewMemStore()
	ctx := context.Background()
	const writers, perWriter = 8, 25
	ch, cancel := s.Subscribe(ctx, "r", 0)
	defer cancel()

	stop := make(chan struct{})
	var churn sync.WaitGroup
	churn.Add(1)
	go func() {
		defer churn.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, c := s.Subscribe(ctx, "r", 0)
				c()
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := s.Append(ctx, []core.Event{{RunID: "r", Type: core.EvSystem}}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	churn.Wait()

	for want := int64(1); want <= writers*perWriter; want++ {
		if ev := <-ch; ev.Sequence != want {
			t.Fatalf("sequence=%d want %d", ev.Sequence, want)
		}
	}
}

func TestExpiredApprovalCannotBeApproved(t *testing.T) {
	s := store.NewMemStore()
	ctx := context.Background()
	id, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, ExpiresAt: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, id, "approved", "", nil); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("approve after expiry err=%v want ErrConflict", err)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, id, "expired", "", nil); err != nil {
		t.Fatalf("expire: %v", err)
	}
	a, err := s.GetApproval(ctx, core.DefaultTenant, id)
	if err != nil || a.Status != "expired" || a.ExpiresAt.IsZero() {
		t.Fatalf("approval=%+v err=%v", a, err)
	}
}

func TestUpdateTaskChangesOnlyStatusOutputAndError(t *testing.T) {
	ctx := context.Background()
	s := store.NewMemStore()
	id, err := s.CreateTask(ctx, store.Task{
		TenantID: core.DefaultTenant, RunID: "run", ParentTaskID: "parent", AgentName: "a",
		Status: core.TaskRunning, Depth: 2, Attempt: 3, Input: json.RawMessage(`{"prompt":"p"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTask(ctx, store.Task{ID: id, TenantID: core.DefaultTenant, Status: core.TaskFailed, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.ListRunTasks(ctx, core.DefaultTenant, "run")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	got := tasks[0]
	if got.Status != core.TaskFailed || got.Error != "boom" || got.ParentTaskID != "parent" || got.Depth != 2 || got.Attempt != 3 || string(got.Input) != `{"prompt":"p"}` {
		t.Fatalf("task=%+v", got)
	}
}

func TestMemStoreMemories(t *testing.T) {
	storetest.Memories(t, store.NewMemStore(), core.SessionID(core.NewID()), core.SessionID(core.NewID()))
}

func TestMemStoreApprovals(t *testing.T) {
	storetest.Approvals(t, store.NewMemStore(), core.RunID(core.NewID()), core.RunID(core.NewID()))
}

func TestMemStoreArtifacts(t *testing.T) {
	storetest.Artifacts(t, store.NewMemStore(), core.SessionID(core.NewID()), core.RunID(core.NewID()))
}
