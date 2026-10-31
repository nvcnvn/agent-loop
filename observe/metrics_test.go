package observe_test

import (
	"strings"
	"testing"

	"github.com/nvcnvn/agent-loop/observe"
)

func TestPrometheusText(t *testing.T) {
	m := observe.New()
	m.Add("agentloop_tool_calls_total", 1, "tool", `say "hi"`, "outcome", "ok")
	m.Add("agentloop_tool_calls_total", 2, "tool", `say "hi"`, "outcome", "ok")
	m.Observe("agentloop_run_duration_seconds", 0.2)
	m.Observe("agentloop_run_duration_seconds", 7)
	m.Gauge("agentloop_runs_queued", func() float64 { return 3 })

	var b strings.Builder
	m.WriteText(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE agentloop_tool_calls_total counter\n",
		`agentloop_tool_calls_total{tool="say \"hi\"",outcome="ok"} 3` + "\n",
		"# TYPE agentloop_run_duration_seconds histogram\n",
		`agentloop_run_duration_seconds_bucket{le="0.1"} 0` + "\n",
		`agentloop_run_duration_seconds_bucket{le="0.25"} 1` + "\n",
		`agentloop_run_duration_seconds_bucket{le="10"} 2` + "\n",
		`agentloop_run_duration_seconds_bucket{le="+Inf"} 2` + "\n",
		"agentloop_run_duration_seconds_sum 7.2\n",
		"agentloop_run_duration_seconds_count 2\n",
		"# TYPE agentloop_runs_queued gauge\nagentloop_runs_queued 3\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestNilMetricsIsNoOp(t *testing.T) {
	var m *observe.Metrics
	m.Add("x", 1)
	m.Observe("y", 1)
	m.Gauge("z", func() float64 { return 1 })
	var b strings.Builder
	m.WriteText(&b)
	if b.Len() != 0 {
		t.Fatalf("nil metrics wrote %q", b.String())
	}
}
