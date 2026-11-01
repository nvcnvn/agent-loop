package core

import (
	"testing"
	"time"
)

func TestLimitsCap(t *testing.T) {
	max := Limits{MaxToolCalls: 10, MaxRuntime: time.Minute, MaxPromptTokens: 1000}
	got := Limits{MaxToolCalls: 1_000_000, MaxRuntime: 24 * time.Hour, MaxModelCalls: 3}.Defaults().Cap(max)
	want := Limits{
		MaxDepth: 4, MaxSubAgents: 16, MaxConcurrent: 4,
		MaxToolCalls: 10, MaxModelCalls: 3, MaxRuntime: time.Minute, MaxPromptTokens: 1000,
	}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestLimitsDefaultsReplaceNegatives(t *testing.T) {
	got := Limits{MaxConcurrent: -1, MaxRuntime: -time.Second, MaxPromptTokens: -5}.Defaults()
	if got.MaxConcurrent != 4 || got.MaxRuntime != 5*time.Minute || got.MaxPromptTokens != 0 {
		t.Fatalf("negatives not replaced: %+v", got)
	}
}

func TestLimitsFillAndNegative(t *testing.T) {
	got := Limits{MaxToolCalls: 5, MaxModelCalls: -1}.Fill(Limits{MaxToolCalls: 9, MaxModelCalls: 7, MaxRuntime: time.Minute})
	if got != (Limits{MaxToolCalls: 5, MaxModelCalls: 7, MaxRuntime: time.Minute}) {
		t.Fatalf("Fill: %+v", got)
	}
	if !(Limits{MaxCostUnits: -1}).Negative() || (Limits{MaxDepth: 1}).Negative() {
		t.Fatal("Negative")
	}
}
