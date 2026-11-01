package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Leases checks a RunStore lets one owner at a time hold a run, until its
// lease passes or it releases the run, and carries cancellation requests.
func Leases(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	sid, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Owner: "a", LeaseUntil: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRun(ctx, core.DefaultTenant, created, "b", time.Minute); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("b claimed a run a created leased: %v", err)
	}
	if err := s.ClaimRun(ctx, core.DefaultTenant, run, "a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRun(ctx, core.DefaultTenant, run, "b", time.Minute); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("b claimed a's run: %v", err)
	}
	if err := s.ClaimRun(ctx, core.TenantID(core.NewID()), run, "b", time.Minute); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("another tenant's claim: %v", err)
	}
	if held, err := s.RenewLeases(ctx, "b", []core.RunID{run}, time.Minute); err != nil || len(held) != 0 {
		t.Fatalf("b renewed a's lease: %v %v", held, err)
	}
	stored, _ := s.GetRun(ctx, core.DefaultTenant, run)
	if stored.Owner != "a" || stored.LeaseUntil.IsZero() {
		t.Fatalf("stored lease: %+v", stored)
	}
	stored.FailureReason = "updated"
	if err := s.UpdateRun(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestCancel(ctx, core.DefaultTenant, run); err != nil {
		t.Fatal(err)
	}
	if held, err := s.RenewLeases(ctx, "a", []core.RunID{run}, time.Minute); err != nil || len(held) != 1 || !held[run] {
		t.Fatalf("a's renewal after a cancel request: %v %v", held, err)
	}
	if err := s.ReleaseRun(ctx, run, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRun(ctx, core.DefaultTenant, run, "b", 50*time.Millisecond); err != nil {
		t.Fatalf("b could not claim a released run: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := s.ClaimRun(ctx, core.DefaultTenant, run, "a", time.Minute); err != nil {
		t.Fatalf("a could not claim a run whose lease passed: %v", err)
	}
	if held, _ := s.RenewLeases(ctx, "b", []core.RunID{run}, time.Minute); len(held) != 0 {
		t.Fatalf("b still holds the run: %v", held)
	}
}
