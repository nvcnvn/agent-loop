package policy_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/policy"
)

func TestToolPolicyAuthorize(t *testing.T) {
	plain := core.ToolDefinition{Name: "echo", SideEffect: core.SideNone}
	gated := core.ToolDefinition{Name: "deploy", RequiresApproval: true}
	external := core.ToolDefinition{Name: "email.send", SideEffect: core.SideWriteExternal}
	cases := []struct {
		name          string
		policy        policy.ToolPolicy
		def           core.ToolDefinition
		allowed, appr bool
	}{
		{"empty policy allows", policy.ToolPolicy{}, plain, true, false},
		{"denylist wins over allowlist", policy.ToolPolicy{Allowlist: []string{"echo"}, Denylist: []string{"echo"}}, plain, false, false},
		{"not in allowlist", policy.ToolPolicy{Allowlist: []string{"other"}}, plain, false, false},
		{"in allowlist", policy.ToolPolicy{Allowlist: []string{"echo"}}, plain, true, false},
		{"definition requires approval", policy.ToolPolicy{}, gated, true, true},
		{"policy requires approval", policy.ToolPolicy{ApprovalRequired: []string{"echo"}}, plain, true, true},
		{"denied before approval", policy.ToolPolicy{Denylist: []string{"deploy"}}, gated, false, false},
		{"side effect needs approval", policy.ToolPolicy{ApproveSideEffects: []core.SideEffectLevel{core.SideWriteExternal}}, external, true, true},
		{"other side effect passes", policy.ToolPolicy{ApproveSideEffects: []core.SideEffectLevel{core.SideWriteExternal}}, plain, true, false},
		{"side effect denied", policy.ToolPolicy{DenySideEffects: []core.SideEffectLevel{core.SideWriteExternal}, ApproveSideEffects: []core.SideEffectLevel{core.SideWriteExternal}}, external, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, appr, reason := tc.policy.Authorize(tc.def)
			if allowed != tc.allowed || appr != tc.appr {
				t.Fatalf("allowed=%v approval=%v reason=%q", allowed, appr, reason)
			}
			if (!allowed || appr) && reason == "" {
				t.Fatal("deny or approval without reason")
			}
		})
	}
}

func TestToolPolicyValidate(t *testing.T) {
	if err := (policy.ToolPolicy{ApproveSideEffects: []core.SideEffectLevel{core.SideDangerous}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (policy.ToolPolicy{DenySideEffects: []core.SideEffectLevel{"writes"}}).Validate(); err == nil {
		t.Fatal("unknown side effect accepted")
	}
}

func TestBudgetReserveOverLimitLeavesUsageUnchanged(t *testing.T) {
	b := policy.NewBudgetManager()
	b.Init("r", core.Limits{MaxToolCalls: 3})
	if err := b.ReserveToolCalls("r", 2); err != nil {
		t.Fatal(err)
	}
	if err := b.ReserveToolCalls("r", 2); !errors.Is(err, core.ErrBudgetExceeded) {
		t.Fatalf("err=%v want ErrBudgetExceeded", err)
	}
	if got := b.Snapshot("r").ToolCalls; got != 2 {
		t.Fatalf("tool calls=%d want 2", got)
	}
	if err := b.ReserveToolCall("r"); err != nil {
		t.Fatalf("last slot: %v", err)
	}
}

func TestBudgetReleaseNeverGoesNegative(t *testing.T) {
	b := policy.NewBudgetManager()
	b.Init("r", core.Limits{})
	if err := b.ReserveSubAgent("r"); err != nil {
		t.Fatal(err)
	}
	b.Release("r", core.Usage{SubAgents: 5, PromptTokens: 10})
	b.Release("unknown", core.Usage{SubAgents: 1})
	if u := b.Snapshot("r"); u.SubAgents != 0 || u.PromptTokens != 0 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestBudgetCommitRecordsThenReportsOverrun(t *testing.T) {
	b := policy.NewBudgetManager()
	b.Init("r", core.Limits{MaxPromptTokens: 100})
	if err := b.Commit("r", core.Usage{PromptTokens: 60}); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit("r", core.Usage{PromptTokens: 60}); !errors.Is(err, core.ErrBudgetExceeded) {
		t.Fatalf("err=%v want ErrBudgetExceeded", err)
	}
	if got := b.Snapshot("r").PromptTokens; got != 120 {
		t.Fatalf("commit must record actual usage: prompt tokens=%d", got)
	}
}

func TestBudgetConcurrentReservationsNeverExceedLimit(t *testing.T) {
	b := policy.NewBudgetManager()
	b.Init("r", core.Limits{MaxModelCalls: 10})
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.ReserveModelCall("r") == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 10 || b.Snapshot("r").ModelCalls != 10 {
		t.Fatalf("granted=%d usage=%d", granted, b.Snapshot("r").ModelCalls)
	}
}

func TestLoopDetector(t *testing.T) {
	d := policy.NewLoopDetector(3)
	for i := 1; i <= 2; i++ {
		if stalled, n := d.Observe("r", "echo", "h"); stalled || n != i {
			t.Fatalf("call %d: stalled=%v n=%d", i, stalled, n)
		}
	}
	if stalled, _ := d.Observe("r", "echo", "other"); stalled {
		t.Fatal("different input must not count")
	}
	if stalled, _ := d.Observe("r2", "echo", "h"); stalled {
		t.Fatal("runs must be isolated")
	}
	if stalled, n := d.Observe("r", "echo", "h"); !stalled || n != 3 {
		t.Fatalf("threshold: stalled=%v n=%d", stalled, n)
	}
	d.Reset("r")
	if stalled, n := d.Observe("r", "echo", "h"); stalled || n != 1 {
		t.Fatalf("after reset: stalled=%v n=%d", stalled, n)
	}
}
