package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/tool"
)

// Acceptance: §Observability and Operations #5 – limits, the worker pool and tool settings come from the config file and environment.
func TestConfigSetsLimitsPoolAndTools(t *testing.T) {
	t.Setenv("AGENT_LOOP_TEST_QUEUE", "7")
	data, err := runtime.ExpandEnv([]byte(`{
		"agents": [{"name": "ag", "tools": ["slow"]}],
		"limits": {"default": {"max_tool_calls": 3}, "max": {"max_tool_calls": 10, "max_runtime": 60000000000}},
		"concurrency": {"max_active_runs": 2, "max_queued_runs": ${AGENT_LOOP_TEST_QUEUE}, "per_tenant": 1, "per_tool": 4},
		"approval_timeout": 1000000000, "idle_warn_after": 0, "max_history_messages": 9,
		"tool_retry": {"max_attempts": 1},
		"tool_policy": {"timeouts": {"slow": 20000000}},
		"workflows": [{"name": "w", "stages": [{"name": "s", "agents": [{"name": "ag", "prompt": "go $notavar"}]}]}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := runtime.ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	prov := model.NewMockProvider(
		model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "slow", Input: json.RawMessage(`{}`)}}},
		model.MockResponse{Content: "done"},
	)
	cfg.Agents[0].Provider, cfg.Agents[0].Model = prov, "m"
	orch, s, sid := setup(t, cfg.Agents)
	cfg.Apply(orch)
	if orch.MaxActiveRuns != 2 || orch.MaxQueuedRuns != 7 || orch.Concurrency != (policy.ConcurrencyLimits{PerTenant: 1, PerTool: 4}) ||
		orch.ApprovalTimeout != time.Second || orch.IdleWarnAfter != 0 || orch.MaxHistoryMessages != 9 || orch.ToolRetry.MaxAttempts != 1 ||
		orch.MaxLimits.MaxToolCalls != 10 || orch.MaxLimits.MaxRuntime != time.Minute || orch.MaxLimits.MaxModelCalls != 256 {
		t.Fatalf("orchestrator not configured: %+v", orch)
	}
	if err := orch.Tools.Register(core.ToolDefinition{Name: "slow", SideEffect: core.SideNone},
		tool.ExecutorFunc(func(ctx context.Context, _ tool.Call) (tool.Result, error) {
			<-ctx.Done()
			return tool.Result{}, ctx.Err()
		})); err != nil {
		t.Fatal(err)
	}
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Template: "w"})
	if err != nil || res.Outputs["s"] != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if run, _ := s.GetRun(context.Background(), core.DefaultTenant, res.RunID); run.Limits.MaxToolCalls != 3 || run.Limits.MaxRuntime != time.Minute {
		t.Fatalf("run limits %+v", run.Limits)
	}
	if last := prov.Calls()[1].Messages; !strings.Contains(last[len(last)-1].Content, "deadline exceeded") {
		t.Fatalf("the tool policy's timeout did not apply: %+v", last[len(last)-1])
	}
	if prompt := prov.Calls()[0].Messages; !strings.Contains(prompt[len(prompt)-1].Content, "$notavar") {
		t.Fatalf("a lone $ was expanded: %q", prompt[len(prompt)-1].Content)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("AGENT_LOOP_TEST_SECRET", `s3$cret`)
	for in, want := range map[string]string{
		`"${AGENT_LOOP_TEST_SECRET}"`:   `"s3$cret"`,
		`"$${AGENT_LOOP_TEST_SECRET}"`:  `"${AGENT_LOOP_TEST_SECRET}"`,
		`"cost $5, ${1X}, $NAME"`:       `"cost $5, ${1X}, $NAME"`,
		`"a${AGENT_LOOP_TEST_SECRET}b"`: `"as3$cretb"`,
	} {
		if got, err := runtime.ExpandEnv([]byte(in)); err != nil || string(got) != want {
			t.Errorf("ExpandEnv(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := runtime.ExpandEnv([]byte(`"${AGENT_LOOP_TEST_UNSET}"`)); err == nil || !strings.Contains(err.Error(), "AGENT_LOOP_TEST_UNSET") {
		t.Fatalf("an unset variable: %v", err)
	}
}

// A workflow's tool policy, which a client may send, only lowers a tool's timeout.
func TestToolTimeoutsOnlyGoDown(t *testing.T) {
	for name, tc := range map[string]struct {
		own, server, workflow time.Duration
	}{
		"own beats a longer workflow":    {own: 20 * time.Millisecond, workflow: time.Hour},
		"server beats a longer workflow": {server: 20 * time.Millisecond, workflow: time.Hour},
		"workflow lowers no timeout":     {workflow: 20 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			prov := model.NewMockProvider(
				model.MockResponse{ToolCalls: []model.ToolCall{{ID: "1", Name: "slow", Input: json.RawMessage(`{}`)}}},
				model.MockResponse{Content: "done"},
			)
			orch, _, sid := setup(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov, Tools: []string{"slow"}}})
			if tc.server > 0 {
				orch.Policy.Timeouts = map[string]time.Duration{"slow": tc.server}
			}
			if err := orch.Tools.Register(core.ToolDefinition{Name: "slow", SideEffect: core.SideNone, Timeout: tc.own},
				tool.ExecutorFunc(func(ctx context.Context, _ tool.Call) (tool.Result, error) {
					select {
					case <-ctx.Done():
						return tool.Result{}, ctx.Err()
					case <-time.After(2 * time.Second):
						return tool.Result{Output: json.RawMessage(`"late"`)}, nil
					}
				})); err != nil {
				t.Fatal(err)
			}
			wf := oneStage("ag")
			wf.ToolPolicy = &policy.ToolPolicy{Timeouts: map[string]time.Duration{"slow": tc.workflow}}
			if _, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: wf}); err != nil {
				t.Fatal(err)
			}
			if last := prov.Calls()[1].Messages; !strings.Contains(last[len(last)-1].Content, "deadline exceeded") {
				t.Fatalf("the call was not cut at the lowest timeout: %+v", last[len(last)-1])
			}
		})
	}
}

func TestApplyKeepsWhatTheFileLeavesOut(t *testing.T) {
	orch, _, _ := setup(t, nil)
	orch.Concurrency.PerTenant, orch.MaxQueuedRuns = 3, 9
	zero := 0
	runtime.Config{Concurrency: &runtime.ConcurrencyConfig{PerTool: 2, MaxActiveRuns: &zero}}.Apply(orch)
	if orch.Concurrency.PerTenant != 3 || orch.Concurrency.PerTool != 2 || orch.MaxActiveRuns != 0 || orch.MaxQueuedRuns != 9 {
		t.Fatalf("pool after apply: %+v active=%d queued=%d", orch.Concurrency, orch.MaxActiveRuns, orch.MaxQueuedRuns)
	}
}

// Acceptance: §Security #4 – the retention policy deletes idle sessions with everything in them.
func TestRetentionPurgesIdleSessions(t *testing.T) {
	cfg, err := runtime.ParseConfig([]byte(`{"agents":[{"name":"ag"}],"retention":{"sessions":3600000000000}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents[0].Provider, cfg.Agents[0].Model = model.NewMockProvider(model.MockResponse{Content: "hi"}), "m"
	orch, s, sid := setup(t, cfg.Agents)
	cfg.Apply(orch)
	res, err := runWorkflow(t, orch, runtime.StartRunInput{SessionID: sid, Workflow: oneStage("ag")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := orch.Purge(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, core.DefaultTenant, sid); err != nil {
		t.Fatalf("a session active within the retention period was purged: %v", err)
	}
	orch.Retention.Sessions = time.Nanosecond
	if err := orch.Purge(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRun(ctx, core.DefaultTenant, res.RunID); err == nil {
		t.Fatal("an idle session's run survived the purge")
	}
}
