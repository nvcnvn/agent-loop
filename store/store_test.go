package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
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
