package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Approvals checks how an ApprovalStore lists approvals by tenant, run,
// status and page, and records who resolved one. Run IDs must be UUIDs of runs the driver accepts approvals for.
func Approvals(t *testing.T, s store.ApprovalStore, run, otherRun core.RunID) {
	t.Helper()
	ctx := context.Background()
	principal := core.PrincipalID(core.NewID())
	create := func(tenant core.TenantID, run core.RunID) core.ApprovalID {
		t.Helper()
		id, err := s.CreateApproval(ctx, store.Approval{TenantID: tenant, RunID: run, Reason: "r"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, second, other := create(core.DefaultTenant, run), create(core.DefaultTenant, run), create(core.DefaultTenant, otherRun)
	create(core.TenantID(core.NewID()), run)
	if err := s.ResolveApproval(ctx, core.DefaultTenant, second, "approved", principal, nil); err != nil {
		t.Fatal(err)
	}
	if a, err := s.GetApproval(ctx, core.DefaultTenant, second); err != nil || a.ResolvedBy != principal {
		t.Fatalf("resolved_by=%q err=%v", a.ResolvedBy, err)
	}
	expired, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: otherRun, ExpiresAt: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(run core.RunID, status string, after core.ApprovalID, limit int) []core.ApprovalID {
		t.Helper()
		list, err := s.ListApprovals(ctx, core.DefaultTenant, store.ApprovalFilter{RunID: run, Status: status, After: after, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		out := []core.ApprovalID{}
		for _, a := range list {
			out = append(out, a.ID)
		}
		return out
	}
	for name, c := range map[string]struct {
		got, want []core.ApprovalID
	}{
		"run, requested":  {ids(run, "requested", "", 0), []core.ApprovalID{first}},
		"run, any":        {ids(run, "", "", 0), []core.ApprovalID{first, second}},
		"tenant, any":     {ids("", "", "", 0), []core.ApprovalID{first, second, other, expired}},
		"approved":        {ids("", "approved", "", 0), []core.ApprovalID{second}},
		"requested, live": {ids("", "requested", "", 0), []core.ApprovalID{first, other}},
		"first page":      {ids("", "", "", 2), []core.ApprovalID{first, second}},
		"next page":       {ids("", "", second, 2), []core.ApprovalID{other, expired}},
	} {
		if len(c.got) != len(c.want) {
			t.Fatalf("%s: got %v want %v", name, c.got, c.want)
		}
		for i := range c.got {
			if c.got[i] != c.want[i] {
				t.Fatalf("%s: got %v want %v", name, c.got, c.want)
			}
		}
	}
}
