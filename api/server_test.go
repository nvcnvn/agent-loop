package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/api"
	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
	"github.com/nvcnvn/agent-loop/tool/memory"
)

func newTestServer(t *testing.T) (*httptest.Server, *runtime.Orchestrator, store.Store) {
	t.Helper()
	s := store.NewMemStore()
	reg := tool.NewRegistry()
	_ = tool.RegisterBuiltins(reg)
	prov := model.NewMockProvider(
		model.MockResponse{Content: "hello world"},
	)
	orch := runtime.NewOrchestrator(s, reg, []runtime.Agent{
		{Name: "echo", Model: "mock", Provider: prov},
	})
	srv := api.NewServer(orch, s)
	srv.Debug = true
	return httptest.NewServer(srv.Handler()), orch, s
}

func TestRequestsAreLogged(t *testing.T) {
	s := store.NewMemStore()
	srv := api.NewServer(runtime.NewOrchestrator(s, tool.NewRegistry(), nil), s)
	var buf bytes.Buffer
	srv.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{}`))
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log is not one JSON record: %q", buf.String())
	}
	if rec["msg"] != "http request" || rec["method"] != "POST" || rec["path"] != "/v1/sessions" || rec["status"] != float64(http.StatusCreated) {
		t.Fatalf("unexpected record: %v", rec)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := `agentloop_http_request_duration_seconds_count{route="POST /v1/sessions",code="201"} 1`
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
		t.Fatalf("status=%d, missing %q in:\n%s", resp.StatusCode, want, body)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()
	body := `{"title":"` + strings.Repeat("x", 2<<20) + `"}`
	resp, err := srv.Client().Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413", resp.StatusCode)
	}
}

type downStore struct{ *store.MemStore }

func (downStore) Ping(context.Context) error { return errors.New("database unreachable") }

func TestReadyzChecksStore(t *testing.T) {
	for _, tc := range []struct {
		store    store.Store
		want     int
		wantBody string
	}{
		{store.NewMemStore(), http.StatusOK, "ok"},
		{downStore{store.NewMemStore()}, http.StatusServiceUnavailable, "store unreachable"},
	} {
		srv := httptest.NewServer(api.NewServer(runtime.NewOrchestrator(tc.store, tool.NewRegistry(), nil), tc.store).Handler())
		resp, err := srv.Client().Get(srv.URL + "/readyz")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want || !strings.Contains(string(body), tc.wantBody) {
			t.Fatalf("%T: status=%d body=%s want %d %q", tc.store, resp.StatusCode, body, tc.want, tc.wantBody)
		}
	}
}

func TestHTTPShutdownClosesStreams(t *testing.T) {
	s := store.NewMemStore()
	gate := gatedProvider{release: make(chan struct{})}
	defer close(gate.release)
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "gated", Model: "m", Provider: gate}})
	hs := api.NewServer(orch, s).HTTPServer("")
	srv := httptest.NewUnstartedServer(hs.Handler)
	srv.Config = hs
	srv.Start()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/"+string(runID)+"/stream", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := hs.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown blocked by an open stream: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("stream did not end cleanly: %v", err)
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	s := store.NewMemStore()
	hs := api.NewServer(runtime.NewOrchestrator(s, tool.NewRegistry(), nil), s).HTTPServer("")
	if hs.ReadHeaderTimeout <= 0 || hs.ReadTimeout <= 0 || hs.IdleTimeout <= 0 {
		t.Fatalf("missing timeouts: %+v", hs)
	}
	if hs.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout=%s would cut SSE streams", hs.WriteTimeout)
	}
}

func TestDebugRoutesOffByDefault(t *testing.T) {
	s := store.NewMemStore()
	srv := httptest.NewServer(api.NewServer(runtime.NewOrchestrator(s, tool.NewRegistry(), nil), s).Handler())
	defer srv.Close()
	for _, path := range []string{"/", "/ui/app.js", "/v1/debug/agents", "/v1/debug/tools"} {
		r, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s status=%d want 404", path, r.StatusCode)
		}
	}
}

func TestRESTSessionAndRun(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()
	c := srv.Client()

	// Create session.
	resp, err := c.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{"title":"demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var sresp struct {
		SessionID core.SessionID `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sresp)
	resp.Body.Close()

	// Start a run.
	body, _ := json.Marshal(map[string]any{
		"session_id": sresp.SessionID,
		"workflow": map[string]any{
			"name": "wf",
			"stages": []map[string]any{{
				"name":   "s",
				"kind":   "sequential",
				"agents": []map[string]any{{"name": "echo", "prompt": "say hi"}},
			}},
		},
	})
	resp, err = c.Post(srv.URL+"/v1/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("start status=%d body=%s", resp.StatusCode, raw)
	}
	var rresp struct {
		RunID core.RunID `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rresp)
	resp.Body.Close()

	// Poll until terminal.
	deadline := time.Now().Add(3 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		r, err := c.Get(srv.URL + "/v1/runs/" + string(rresp.RunID))
		if err != nil {
			t.Fatal(err)
		}
		var run store.Run
		_ = json.NewDecoder(r.Body).Decode(&run)
		r.Body.Close()
		status = string(run.Status)
		if run.Status == core.RunSucceeded || run.Status == core.RunFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != string(core.RunSucceeded) {
		t.Fatalf("final status=%s", status)
	}

	// List events; verify at least one public event exists.
	r, err := c.Get(srv.URL + "/v1/runs/" + string(rresp.RunID) + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var ebody struct {
		Events []core.Event `json:"events"`
	}
	_ = json.NewDecoder(r.Body).Decode(&ebody)
	if len(ebody.Events) == 0 {
		t.Fatalf("no events returned")
	}
}

func TestSSEStream(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()
	c := srv.Client()

	// Session + run.
	resp, _ := c.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	var sresp struct {
		SessionID core.SessionID `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sresp)
	resp.Body.Close()

	body, _ := json.Marshal(map[string]any{
		"session_id": sresp.SessionID,
		"workflow": map[string]any{
			"stages": []map[string]any{{
				"name":   "s",
				"agents": []map[string]any{{"name": "echo", "prompt": "x"}},
			}},
		},
	})
	resp, _ = c.Post(srv.URL+"/v1/runs", "application/json", bytes.NewReader(body))
	var rresp struct {
		RunID core.RunID `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rresp)
	resp.Body.Close()

	// Open SSE.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/"+string(rresp.RunID)+"/stream", nil)
	req.Header.Set("X-Role", "member")
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("sse status=%d", r.StatusCode)
	}
	if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type=%s", got)
	}

	stream, _ := io.ReadAll(r.Body)
	if strings.Contains(string(stream), core.EvModelStarted) || strings.Contains(string(stream), core.EvTaskSpawned) {
		t.Fatalf("SSE exposed internal event: %s", stream)
	}
	if !strings.Contains(string(stream), `"status":"succeeded"`) {
		t.Fatalf("did not see terminal succeeded event over SSE: %s", stream)
	}
	if !strings.Contains(string(stream), `"actor_id":"`+string(core.DefaultPrincipal)+`"`) {
		t.Fatalf("SSE does not name who started the run: %s", stream)
	}
	if strings.Contains(string(stream), "agent_message_delta") {
		t.Fatalf("deltas sent without ?deltas=1: %s", stream)
	}
	envelopes := map[string]map[string]any{}
	var event string
	for line := range strings.Lines(string(stream)) {
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			event = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			var env map[string]any
			if err := json.Unmarshal([]byte(v), &env); err != nil {
				t.Fatal(err)
			}
			envelopes[event] = env
		}
	}
	msg, state := envelopes[core.EvAgentMessage], envelopes[core.EvRunStateChanged]
	if msg["event_id"] == "" || msg["event_id"] == nil || msg["correlation_id"] != string(rresp.RunID) || msg["parent_event_id"] == nil {
		t.Fatalf("agent message envelope lacks its IDs: %v", msg)
	}
	if _, ok := state["parent_event_id"]; ok || state["causation_id"] != nil || state["correlation_id"] != string(rresp.RunID) {
		t.Fatalf("run state envelope: %v", state)
	}
}

// gatedStreamer streams "done" as two deltas once released.
type gatedStreamer struct{ gatedProvider }

func (g gatedStreamer) ChatStream(ctx context.Context, req model.ChatRequest, onDelta func(model.Delta)) (model.ChatResponse, error) {
	resp, err := g.Chat(ctx, req)
	if err == nil {
		onDelta(model.Delta{Content: "do"})
		onDelta(model.Delta{Content: "ne", Thinking: "secret"})
	}
	return resp, err
}

func TestSSEStreamDeltas(t *testing.T) {
	s := store.NewMemStore()
	gate := gatedStreamer{gatedProvider{release: make(chan struct{})}}
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "gated", Model: "m", Provider: gate}})
	srv := httptest.NewServer(api.NewServer(orch, s).Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/"+string(runID)+"/stream?deltas=1", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	close(gate.release)
	body, _ := io.ReadAll(resp.Body)
	stream := string(body)

	first := strings.Index(stream, "event: agent_message_delta\ndata: ")
	second := strings.Index(stream, `"content":"ne"`)
	final := strings.Index(stream, "event: "+core.EvAgentMessage)
	if first < 0 || !strings.Contains(stream, `"content":"do"`) || second < 0 || final < second {
		t.Fatalf("want deltas do, ne before %s:\n%s", core.EvAgentMessage, stream)
	}
	if strings.Contains(stream, "secret") {
		t.Fatalf("thinking leaked into deltas:\n%s", stream)
	}
}

type gatedProvider struct{ release chan struct{} }

func (g gatedProvider) Name() string { return "gated" }
func (g gatedProvider) Capabilities(context.Context, string) (model.Capabilities, error) {
	return model.Capabilities{}, nil
}
func (g gatedProvider) Chat(ctx context.Context, _ model.ChatRequest) (model.ChatResponse, error) {
	select {
	case <-ctx.Done():
		return model.ChatResponse{}, ctx.Err()
	case <-g.release:
		return model.ChatResponse{Message: model.Message{Role: model.RoleAssistant, Content: "done"}}, nil
	}
}

// Acceptance: §Security #3 – who canceled a run, and why, is audited.
func TestCancelRunIsAudited(t *testing.T) {
	s := store.NewMemStore()
	gate := gatedProvider{release: make(chan struct{})}
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "gated", Model: "m", Provider: gate}})
	srv := httptest.NewServer(api.NewServer(orch, s).Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{{
		Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/runs/"+string(runID)+"/cancel", strings.NewReader(`{"reason":"duplicate request"}`))
	req.Header.Set("X-Principal-ID", "00000000-0000-0000-0000-0000000000c3")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status=%d", resp.StatusCode)
	}
	if res, _ := orch.Wait(ctx, core.DefaultTenant, runID); res.Status != core.RunCanceled {
		t.Fatalf("status=%s", res.Status)
	}
	events, _ := s.ListRunEvents(ctx, core.DefaultTenant, runID, 0, 0)
	for _, ev := range events {
		if ev.ActorID == "00000000-0000-0000-0000-0000000000c3" && strings.Contains(string(ev.Payload), `"reason":"duplicate request"`) {
			return
		}
	}
	t.Fatal("no audit event names who canceled the run and why")
}

// Acceptance: §Security #1 – cross-tenant run cancellation.
func TestCancelRunRejectsOtherTenant(t *testing.T) {
	s := store.NewMemStore()
	gate := gatedProvider{release: make(chan struct{})}
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "gated", Model: "m", Provider: gate}})
	srv := httptest.NewServer(api.NewServer(orch, s).Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name:   "s",
			Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/runs/"+string(runID)+"/cancel", nil)
	req.Header.Set("X-Tenant-ID", "00000000-0000-0000-0000-0000000000bb")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant cancel status=%d want 404", resp.StatusCode)
	}
	close(gate.release)
	res, err := orch.Wait(ctx, core.DefaultTenant, runID)
	if err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("status=%s err=%v want succeeded", res.Status, err)
	}

	resp, err = srv.Client().Post(srv.URL+"/v1/runs/"+string(runID)+"/cancel", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("cancel of a finished run status=%d want 409", resp.StatusCode)
	}

	for path, want := range map[string]int{
		"/v1/runs/" + string(runID) + "/resume": http.StatusConflict,
		"/v1/runs/missing/resume":               http.StatusNotFound,
	} {
		resp, err = srv.Client().Post(srv.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("POST %s status=%d want %d", path, resp.StatusCode, want)
		}
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/runs/"+string(runID)+"/resume", nil)
	req.Header.Set("X-Tenant-ID", "00000000-0000-0000-0000-0000000000bb")
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant resume status=%d want 404", resp.StatusCode)
	}
}

// Regression: the live part of a stream was not scoped to the caller's tenant.
func TestSSEStreamRejectsOtherTenant(t *testing.T) {
	s := store.NewMemStore()
	gate := gatedProvider{release: make(chan struct{})}
	defer close(gate.release)
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "gated", Model: "m", Provider: gate}})
	srv := httptest.NewServer(api.NewServer(orch, s).Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		SessionID: sid,
		Workflow:  runtime.Workflow{Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "gated", Prompt: "x"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/"+string(runID)+"/stream", nil)
	req.Header.Set("X-Tenant-ID", "00000000-0000-0000-0000-0000000000bb")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant stream status=%d want 404", resp.StatusCode)
	}
}

func TestStartRunOverloaded(t *testing.T) {
	s := store.NewMemStore()
	gate := gatedProvider{release: make(chan struct{})}
	defer close(gate.release)
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "gated", Model: "m", Provider: gate}})
	orch.MaxActiveRuns, orch.MaxQueuedRuns = 1, 0
	srv := httptest.NewServer(api.NewServer(orch, s).Handler())
	defer srv.Close()
	sid, _ := s.CreateSession(context.Background(), store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	body := `{"session_id":"` + string(sid) + `","workflow":{"stages":[{"name":"s","agents":[{"name":"gated","prompt":"x"}]}]}}`
	for i, want := range []int{http.StatusAccepted, http.StatusServiceUnavailable} {
		resp, err := srv.Client().Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("run %d status=%d want %d", i, resp.StatusCode, want)
		}
		if want == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") == "" {
			t.Fatal("503 without Retry-After")
		}
	}
}

func TestStartRunFromTemplate(t *testing.T) {
	srv, orch, s := newTestServer(t)
	defer srv.Close()
	orch.Workflows["greet"] = runtime.Workflow{Name: "greet", Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "echo", Prompt: "hi"}}}}}
	sid, _ := s.CreateSession(context.Background(), store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	for body, want := range map[string]int{
		`{"session_id":"` + string(sid) + `","template":"greet"}`:   http.StatusAccepted,
		`{"session_id":"` + string(sid) + `","template":"missing"}`: http.StatusBadRequest,
		`{"session_id":"` + string(sid) + `"}`:                      http.StatusBadRequest,
	} {
		resp, err := srv.Client().Post(srv.URL+"/v1/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var got startResp
		_ = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: status=%d want %d", body, resp.StatusCode, want)
		}
		if want != http.StatusAccepted {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := orch.Wait(ctx, core.DefaultTenant, got.RunID)
		cancel()
		if err != nil || res.Outputs["s"] != "hello world" {
			t.Fatalf("result=%+v err=%v", res, err)
		}
	}
}

type startResp struct {
	RunID core.RunID `json:"run_id"`
}

func TestSSEStreamOfFinishedRunCloses(t *testing.T) {
	srv, orch, s := newTestServer(t)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sid, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{
		TenantID:  core.DefaultTenant,
		SessionID: sid,
		Workflow: runtime.Workflow{Stages: []runtime.Stage{{
			Name: "s", Agents: []runtime.AgentStep{{Name: "echo", Prompt: "x"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/"+string(runID)+"/stream", nil)
	r, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	stream, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("stream did not close after replaying a terminal run: %v", err)
	}
	if !strings.Contains(string(stream), `"status":"succeeded"`) {
		t.Fatalf("history missing terminal event: %s", stream)
	}
}

func TestSSEStreamOfResumedRunPassesEarlierFailure(t *testing.T) {
	s := store.NewMemStore()
	prov := model.NewMockProvider(model.MockResponse{Err: errors.New("down")})
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "a", Model: "m", Provider: prov}})
	srv := httptest.NewServer(api.NewServer(orch, s).Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "a", Prompt: "x"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := orch.Wait(ctx, core.DefaultTenant, runID); res.Status != core.RunFailed {
		t.Fatalf("status=%s", res.Status)
	}
	prov.Push(model.MockResponse{Content: "ok"})
	resp, err := srv.Client().Post(srv.URL+"/v1/runs/"+string(runID)+"/resume", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("resume: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/"+string(runID)+"/stream", nil)
	r, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	stream, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	failed := strings.Index(string(stream), `"status":"failed"`)
	succeeded := strings.Index(string(stream), `"status":"succeeded"`)
	if failed < 0 || succeeded < failed {
		t.Fatalf("stream stopped at the earlier failure: %s", stream)
	}
}

func TestListRunEventsFiltersInternalVisibility(t *testing.T) {
	srv, _, s := newTestServer(t)
	defer srv.Close()
	ctx := context.Background()
	sid, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	rid, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Append(ctx, []core.Event{
		{TenantID: core.DefaultTenant, SessionID: sid, RunID: rid, Type: core.EvModelStarted, Visibility: core.VisibilityInternal, HiddenPayload: json.RawMessage(`{"request":{}}`)},
		{TenantID: core.DefaultTenant, SessionID: sid, RunID: rid, Type: core.EvApprovalReq, Visibility: core.VisibilityParticipant},
		{TenantID: core.DefaultTenant, SessionID: sid, RunID: rid, Type: core.EvSystem, Visibility: core.VisibilityAudit},
		{TenantID: core.DefaultTenant, SessionID: sid, RunID: rid, Type: core.EvAgentTrace, Visibility: core.VisibilityHiddenReasoning},
	})
	if err != nil {
		t.Fatal(err)
	}

	events := func(role string) []core.Event {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/runs/"+string(rid)+"/events", nil)
		req.Header.Set("X-Role", role)
		r, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var body struct {
			Events []core.Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Events
	}
	if got := events("member"); len(got) != 1 || got[0].Type != core.EvApprovalReq {
		t.Fatalf("member's events: %+v", got)
	}
	operator := events("operator")
	if len(operator) != 3 || slices.ContainsFunc(operator, func(e core.Event) bool {
		return e.Visibility == core.VisibilityHiddenReasoning || len(e.HiddenPayload) > 0
	}) {
		t.Fatalf("operator's events: %+v", operator)
	}
	admin := events("admin")
	if len(admin) != 4 || len(admin[0].HiddenPayload) == 0 {
		t.Fatalf("admin's events: %+v", admin)
	}
}

func TestResolveApprovalEndpoint(t *testing.T) {
	srv, _, s := newTestServer(t)
	defer srv.Close()
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	rid, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunAwaitingApproval})
	aid, err := s.CreateApproval(ctx, store.Approval{
		TenantID: core.DefaultTenant,
		RunID:    rid,
		Reason:   "debug console gate",
		Request:  json.RawMessage(`{"tool":"echo"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Post(srv.URL+"/v1/approvals/"+string(aid), "application/json", strings.NewReader(`{"decision":"maybe"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid decision status=%d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/approvals/"+string(aid), strings.NewReader(`{"decision":"approved","payload":{"source":"debug_console"}}`))
	req.Header.Set("X-Principal-ID", "00000000-0000-0000-0000-0000000000a9")
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve status=%d", resp.StatusCode)
	}

	approval, err := s.GetApproval(ctx, core.DefaultTenant, aid)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Status != "approved" {
		t.Fatalf("status=%s want approved", approval.Status)
	}
	if string(approval.Resolution) != `{"source":"debug_console"}` {
		t.Fatalf("resolution=%s", approval.Resolution)
	}
	if approval.ResolvedAt.IsZero() || approval.ResolvedBy != "00000000-0000-0000-0000-0000000000a9" {
		t.Fatalf("resolved_at=%v resolved_by=%q", approval.ResolvedAt, approval.ResolvedBy)
	}
}

func TestListApprovals(t *testing.T) {
	srv, _, s := newTestServer(t)
	defer srv.Close()
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant})
	run, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid})
	open, _ := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: run, Reason: "tool requires approval", Request: json.RawMessage(`{"tool":"refund"}`)})
	done, _ := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: run})
	_ = s.ResolveApproval(ctx, core.DefaultTenant, done, "rejected", "", nil)
	var next string
	list := func(query, tenant string) (int, []map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/approvals"+query, nil)
		if tenant != "" {
			req.Header.Set("X-Tenant-ID", tenant)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Approvals []map[string]any
			NextAfter string `json:"next_after"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		next = body.NextAfter
		return resp.StatusCode, body.Approvals
	}
	code, got := list("", "")
	if code != http.StatusOK || len(got) != 1 || got[0]["approval_id"] != string(open) || got[0]["run_id"] != string(run) ||
		got[0]["status"] != "requested" || got[0]["reason"] != "tool requires approval" {
		t.Fatalf("pending: code=%d %v", code, got)
	}
	if _, got := list("?status=all&run_id="+string(run), ""); len(got) != 2 {
		t.Fatalf("all: %v", got)
	}
	if _, got := list("?status=rejected", ""); len(got) != 1 || got[0]["approval_id"] != string(done) {
		t.Fatalf("rejected: %v", got)
	}
	if _, got := list("?status=all&limit=1", ""); len(got) != 1 || next != string(open) {
		t.Fatalf("first page: %v next=%q", got, next)
	}
	if _, got := list("?status=all&limit=1&after="+next, ""); len(got) != 1 || got[0]["approval_id"] != string(done) {
		t.Fatalf("second page: %v", got)
	}
	for _, bad := range []string{"?status=aproved", "?limit=0", "?limit=1000", "?after=x"} {
		if code, _ := list(bad, ""); code != http.StatusBadRequest {
			t.Fatalf("%s: code=%d", bad, code)
		}
	}
	other := core.NewID()
	if _, got := list("", other); len(got) != 0 {
		t.Fatalf("another tenant sees approvals: %v", got)
	}
	if code, _ := list("?run_id="+string(run), other); code != http.StatusNotFound {
		t.Fatalf("another tenant's run: code=%d", code)
	}
}

func TestArtifactEndpoints(t *testing.T) {
	srv, _, s := newTestServer(t)
	defer srv.Close()
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant})
	run, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid})
	id, _ := s.PutArtifact(ctx, store.Artifact{TenantID: core.DefaultTenant, SessionID: sid, RunID: run, Producer: "artifact.save",
		Kind: "report", Name: "rapport é.html", MimeType: "text/html", Content: []byte("<script>alert(1)</script>")})
	get := func(path, tenant string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if tenant != "" {
			req.Header.Set("X-Tenant-ID", tenant)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	var list struct{ Artifacts []map[string]any }
	_ = json.NewDecoder(get("/v1/runs/"+string(run)+"/artifacts", "").Body).Decode(&list)
	if len(list.Artifacts) != 1 || list.Artifacts[0]["artifact_id"] != id || list.Artifacts[0]["producer"] != "artifact.save" ||
		list.Artifacts[0]["size"] != 25.0 || list.Artifacts[0]["content"] != nil {
		t.Fatalf("list=%v", list.Artifacts)
	}
	resp := get("/v1/artifacts/"+id, "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "<script>alert(1)</script>" || resp.Header.Get("Content-Type") != "text/html" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment;") || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("download: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	again, _ := s.PutArtifact(ctx, store.Artifact{TenantID: core.DefaultTenant, SessionID: sid, RunID: run, Kind: "report", Name: "rapport é.html", Content: []byte("v2")})
	var versions struct{ Artifacts []store.Artifact }
	_ = json.NewDecoder(get("/v1/artifacts/"+again+"/versions", "").Body).Decode(&versions)
	if len(versions.Artifacts) != 2 || versions.Artifacts[0].ID != id || versions.Artifacts[1].ID != again || versions.Artifacts[1].Version != 2 {
		t.Fatalf("versions=%+v", versions.Artifacts)
	}
	other := core.NewID()
	for _, path := range []string{"/v1/artifacts/" + id, "/v1/artifacts/" + id + "/versions", "/v1/runs/" + string(run) + "/artifacts", "/v1/artifacts/missing", "/v1/artifacts/missing/versions"} {
		tenant := other
		if strings.HasPrefix(path, "/v1/artifacts/missing") {
			tenant = ""
		}
		if code := get(path, tenant).StatusCode; code != http.StatusNotFound {
			t.Fatalf("%s as another tenant: %d", path, code)
		}
	}
}

func TestResolveApprovalConflicts(t *testing.T) {
	srv, _, s := newTestServer(t)
	defer srv.Close()
	sid, _ := s.CreateSession(context.Background(), store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	rid, _ := s.CreateRun(context.Background(), store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning})
	expired, err := s.CreateApproval(context.Background(), store.Approval{TenantID: core.DefaultTenant, RunID: rid, ExpiresAt: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{
		"/v1/approvals/" + string(expired): http.StatusConflict,
		"/v1/approvals/missing":            http.StatusNotFound,
	} {
		resp, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader(`{"decision":"approved"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("POST %s status=%d want %d", path, resp.StatusCode, want)
		}
	}
}

func TestSessionDebugListEndpoints(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()
	c := srv.Client()

	resp, err := c.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{"title":"debug-list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		SessionID core.SessionID `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	resp, err = c.Post(srv.URL+"/v1/sessions/"+string(created.SessionID)+"/messages", "application/json", strings.NewReader(`{"role":"human","content":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	r, err := c.Get(srv.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var sessions struct {
		Sessions []store.Session `json:"sessions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].ID != created.SessionID {
		t.Fatalf("unexpected sessions: %+v", sessions.Sessions)
	}

	r, err = c.Get(srv.URL + "/v1/sessions/" + string(created.SessionID) + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var messages struct {
		Messages []store.MessageProjection `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	if len(messages.Messages) != 1 || messages.Messages[0].Content != "hello" {
		t.Fatalf("unexpected messages: %+v", messages.Messages)
	}

	r, err = c.Get(srv.URL + "/v1/sessions/" + string(created.SessionID) + "/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("runs status=%d", r.StatusCode)
	}
}

func TestDebugConsoleServed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()
	c := srv.Client()

	r, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("console status=%d", r.StatusCode)
	}
	raw, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(raw), "Debug Console") {
		t.Fatalf("console body missing title: %s", raw)
	}

	r, err = c.Get(srv.URL + "/ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("asset status=%d", r.StatusCode)
	}
	raw, _ = io.ReadAll(r.Body)
	if !strings.Contains(string(raw), "Last-Event-ID") {
		t.Fatalf("asset body missing SSE client")
	}
}

func TestDebugSetupEndpoints(t *testing.T) {
	srv, _, _ := newTestServer(t)
	defer srv.Close()
	c := srv.Client()

	r, err := c.Get(srv.URL + "/v1/debug/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("agents status=%d", r.StatusCode)
	}
	var agents struct {
		Agents []struct {
			Name         string `json:"name"`
			Model        string `json:"model"`
			MaxToolLoops int    `json:"max_tool_loops"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&agents); err != nil {
		t.Fatal(err)
	}
	if len(agents.Agents) != 1 || agents.Agents[0].Name != "echo" || agents.Agents[0].Model != "mock" {
		t.Fatalf("unexpected agents: %+v", agents.Agents)
	}

	r, err = c.Get(srv.URL + "/v1/debug/tools")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("tools status=%d", r.StatusCode)
	}
	var tools struct {
		Tools []struct {
			Name        string `json:"name"`
			Concurrency string `json:"concurrency"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&tools); err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 4 || tools.Tools[0].Name == "" {
		t.Fatalf("unexpected tools: %+v", tools.Tools)
	}
}

func FuzzRESTBodies(f *testing.F) {
	s := store.NewMemStore()
	srv := api.NewServer(runtime.NewOrchestrator(s, tool.NewRegistry(), nil), s)
	srv.Log = slog.New(slog.DiscardHandler)
	h := srv.Handler()
	sid, err := s.CreateSession(context.Background(), store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		f.Fatal(err)
	}
	rid, err := s.CreateRun(context.Background(), store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunSucceeded})
	if err != nil {
		f.Fatal(err)
	}
	paths := []string{"/v1/sessions", "/v1/sessions/" + string(sid) + "/messages", "/v1/approvals/missing", "/v1/runs", "/v1/runs/" + string(rid) + "/cancel",
		"/v1/sessions/" + string(sid) + "/participants/" + string(core.DefaultPrincipal), "/v1/sessions/" + string(sid) + "/memories", "/v1/agents/fuzzed"}
	srv.Memory = &memory.Tools{Store: s, Embedder: model.NewMockProvider(), Model: "bow"}
	f.Add(uint8(0), []byte(`{"title":"demo"}`))
	f.Add(uint8(0), []byte(``))
	f.Add(uint8(1), []byte(`{"role":"user","content":"hi"}`))
	f.Add(uint8(1), []byte(`{"role":"user","content":"hi","run":{"template":"t","input":{"message":"x"},"limits":{"max_tool_calls":-1}}}`))
	f.Add(uint8(1), []byte(`{"content":"hi","run":null}`))
	f.Add(uint8(2), []byte(`{"decision":"approved","payload":{"x":1}}`))
	f.Add(uint8(2), []byte(`{"decision":1}`))
	f.Add(uint8(3), []byte(`{"session_id":"x","workflow":{"name":"wf","stages":[{"name":"a","kind":"sequential","agents":[{"name":"missing","prompt":"p"}]}]}}`))
	f.Add(uint8(3), []byte(`{"session_id":"`+string(sid)+`","workflow":{"stages":[{"name":"a","kind":"parallel","agents":[{"name":"missing"}]}]},"limits":{"max_concurrent":-1,"max_tool_calls":-1}}`))
	f.Add(uint8(3), []byte(`{"session_id":"`+string(sid)+`","template":"missing","workflow":{"stages":[{"name":"a","depends_on":["a"],"agents":[{"name":"x"}]}]}}`))
	f.Add(uint8(3), []byte(`{"session_id":"`+string(sid)+`","workflow":{"tool_policy":{"deny_side_effects":["x"],"allowlist":[1]},"stages":[{"name":"a","agents":[{"name":"x"}]}]}}`))
	f.Add(uint8(3), []byte(`{"session_id":"`+string(sid)+`","workflow":{"input_schema":{"$ref":"file:///etc/passwd"},"stages":[{"name":"a","hand_back":{"when":"(","to":"b"},"output_schema":{"type":5},"agents":[{"name":"x"}]}]}}`))
	f.Add(uint8(3), []byte(`{"session_id":"`+string(sid)+`","images":{"p":{"mime_type":"image/png","data":"not base64!"}},"workflow":{"stages":[{"name":"a","agents":[{"name":"x","images":["p","q"]}]}]}}`))
	f.Add(uint8(4), []byte(`{"reason":"`+strings.Repeat("x", 2000)+`"}`))
	f.Add(uint8(4), []byte(`{"reason":7}`))
	f.Add(uint8(7), []byte(`{"instructions":"x","tools":["echo"],"children":["x"],"timeout":-1}`))
	f.Add(uint8(7), []byte(`{"max_tool_loops":"many"}`))
	f.Add(uint8(5), []byte(`{"role":"viewer"}`))
	f.Add(uint8(5), []byte(`{"role":"boss"}`))
	f.Add(uint8(5), []byte(`{"role":["owner"]}`))
	f.Add(uint8(6), []byte(`{"name":"a.md","content":"one\n\ntwo","scope":"session"}`))
	f.Add(uint8(6), []byte(`{"content":"","scope":"tenant"}`))
	f.Add(uint8(6), []byte(`{"content":"x","scope":"project"}`))
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		path := paths[int(which)%len(paths)]
		method := http.MethodPost
		if strings.Contains(path, "/participants/") || strings.HasPrefix(path, "/v1/agents/") {
			method = http.MethodPut
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(body)))
		if rec.Code >= 500 {
			t.Fatalf("POST %s %q: status %d: %s", path, body, rec.Code, rec.Body)
		}
	})
}

func TestReExecuteEndpoint(t *testing.T) {
	srv, orch, s := newTestServer(t)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	runID, err := orch.StartRun(ctx, runtime.StartRunInput{SessionID: sid, Workflow: runtime.Workflow{Stages: []runtime.Stage{
		{Name: "s", Agents: []runtime.AgentStep{{Name: "echo", Prompt: "x"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Wait(ctx, core.DefaultTenant, runID); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Post(srv.URL+"/v1/runs/"+string(runID)+"/reexecute", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res runtime.Result
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || res.Status != core.RunSucceeded || res.Outputs["s"] != "hello world" || res.RunID == runID {
		t.Fatalf("status=%d result=%+v", resp.StatusCode, res)
	}

	for _, tenant := range []string{"", "00000000-0000-0000-0000-0000000000bb"} {
		path := "/v1/runs/" + string(runID) + "/reexecute"
		if tenant == "" {
			path = "/v1/runs/missing/reexecute"
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		if tenant != "" {
			req.Header.Set("X-Tenant-ID", tenant)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s tenant=%q status=%d want 404", path, tenant, resp.StatusCode)
		}
	}
}

func TestListMessagesHidesInternalMessages(t *testing.T) {
	srv, _, s := newTestServer(t)
	defer srv.Close()
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	_ = s.AddMessage(ctx, store.MessageProjection{SessionID: sid, Role: "user", Visibility: core.VisibilityParticipant, Content: "hi"})
	_ = s.AddMessage(ctx, store.MessageProjection{SessionID: sid, Role: "snapshot", Visibility: core.VisibilityInternal, Content: `{"summary":"secret"}`})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions/"+string(sid)+"/messages", nil)
	req.Header.Set("X-Role", "member")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"hi"`) || strings.Contains(string(body), "secret") {
		t.Fatalf("messages: %s", body)
	}
}

func FuzzRESTQueries(f *testing.F) {
	s := store.NewMemStore()
	srv := api.NewServer(runtime.NewOrchestrator(s, tool.NewRegistry(), nil), s)
	srv.Log = slog.New(slog.DiscardHandler)
	h := srv.Handler()
	ctx := context.Background()
	sid, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		f.Fatal(err)
	}
	rid, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunSucceeded})
	if err != nil {
		f.Fatal(err)
	}
	ev := func(vis core.Visibility, payload string) core.Event {
		return core.Event{TenantID: core.DefaultTenant, SessionID: sid, RunID: rid, Type: core.EvRunStateChanged, Visibility: vis, Payload: json.RawMessage(payload)}
	}
	if _, err := s.Append(ctx, []core.Event{
		ev(core.VisibilityPublic, `{"status":"running"}`), ev(core.VisibilityInternal, `{}`),
		ev(core.VisibilityParticipant, `{}`), ev(core.VisibilityPublic, `{"status":"succeeded"}`),
	}); err != nil {
		f.Fatal(err)
	}
	f.Add(uint8(0), "status=all&limit=100&after=00000000-0000-0000-0000-000000000001", "", "")
	f.Add(uint8(0), "limit=-1&run_id=%00", "", "")
	f.Add(uint8(1), "after=-5&limit=99999999999999999999", "", "")
	f.Add(uint8(1), "after=2&limit=1", "", "")
	f.Add(uint8(2), "after=x&deltas=1", "1", "")
	f.Add(uint8(2), "", "9", "")
	f.Add(uint8(2), "after=2", "\xa8", "")
	f.Add(uint8(3), "", "", "not-a-uuid")
	f.Add(uint8(3), "", "", "../../sessions")
	f.Fuzz(func(t *testing.T, which uint8, query, lastEventID, runID string) {
		paths := []string{"/v1/approvals", "/v1/runs/" + string(rid) + "/events", "/v1/runs/" + string(rid) + "/stream", "/v1/runs/" + url.PathEscape(runID) + "/events"}
		path := paths[int(which)%len(paths)]
		// A stream stays open while its run may still emit, so it must end with the request.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		req.URL.RawQuery = query
		req.Header.Set("Last-Event-ID", lastEventID)
		req.Header.Set("X-Role", "member")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("GET %s?%s: status %d: %s", path, query, rec.Code, rec.Body)
		}
		if rec.Code != http.StatusOK {
			return
		}
		after, _ := strconv.ParseInt(req.URL.Query().Get("after"), 10, 64)
		switch path {
		case paths[1]:
			var body struct{ Events []core.Event }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			for _, e := range body.Events {
				if e.Visibility == core.VisibilityInternal || e.Sequence <= after {
					t.Fatalf("?%s served event %d (%s)", query, e.Sequence, e.Visibility)
				}
			}
		case paths[2]:
			if v, err := strconv.ParseInt(lastEventID, 10, 64); err == nil {
				after = v
			}
			for line := range strings.Lines(rec.Body.String()) {
				if id, ok := strings.CutPrefix(strings.TrimSpace(line), "id: "); ok {
					if n, _ := strconv.ParseInt(id, 10, 64); n <= after || n == 2 {
						t.Fatalf("Last-Event-ID %q ?%s streamed event %d", lastEventID, query, n)
					}
				}
			}
		}
	})
}

// Acceptance: §Memory and Retrieval #7 – documents and text artifacts are ingested over REST.
func TestIngestIntoMemory(t *testing.T) {
	s := store.NewMemStore()
	srv := api.NewServer(runtime.NewOrchestrator(s, tool.NewRegistry(), nil), s)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal, Project: "apollo"})
	text, _ := s.PutArtifact(ctx, store.Artifact{TenantID: core.DefaultTenant, SessionID: sid, Kind: "report", Name: "notes.md", MimeType: "text/markdown", Content: []byte("Ship on Friday.")})
	binary, _ := s.PutArtifact(ctx, store.Artifact{TenantID: core.DefaultTenant, SessionID: sid, Kind: "file", Name: "logo.png", MimeType: "image/png", Content: []byte{0x89, 'P', 'N', 'G'}})
	post := func(path, role, body string, want int) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		req.Header.Set("X-Role", role)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("POST %s as %s: %d want %d: %s", path, role, resp.StatusCode, want, out)
		}
	}
	docs := "/v1/sessions/" + string(sid) + "/memories"
	post(docs, "admin", `{"name":"a.md","content":"x"}`, http.StatusNotImplemented)
	srv.Memory = &memory.Tools{Store: s, Embedder: model.NewMockProvider(), Model: "bow"}
	post(docs, "admin", `{"name":"handbook.md","content":"Refunds take five days.\n\nShipping is free.","scope":"project"}`, http.StatusCreated)
	post(docs, "member", `{"content":"Everyone must see this.","scope":"tenant"}`, http.StatusForbidden)
	post(docs, "admin", `{"content":"x","scope":"workflow"}`, http.StatusBadRequest)
	post("/v1/artifacts/"+text+"/memories", "admin", ``, http.StatusCreated)
	post("/v1/artifacts/"+binary+"/memories", "admin", `{}`, http.StatusUnsupportedMediaType)
	post("/v1/artifacts/"+core.NewID()+"/memories", "admin", `{}`, http.StatusNotFound)
	post(docs, "viewer", `{"content":"x"}`, http.StatusForbidden)
	post("/v1/sessions", "admin", `{"title":"t","project":"  apollo  "}`, http.StatusCreated)
	post("/v1/sessions", "admin", `{"project":"`+strings.Repeat("p", 201)+`"}`, http.StatusBadRequest)
	resp, err := ts.Client().Get(ts.URL + "/v1/sessions?project=apollo")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct{ Sessions []store.Session }
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if len(listed.Sessions) != 2 || listed.Sessions[0].Project != "apollo" || listed.Sessions[1].Project != "apollo" {
		t.Fatalf("project sessions: %+v", listed.Sessions)
	}

	vec, _ := model.NewMockProvider().Embed(ctx, "bow", []string{"ship friday"})
	hits, _ := s.SearchMemories(ctx, store.MemoryQuery{TenantID: core.DefaultTenant, Embedding: vec[0], EmbedModel: "bow", SessionID: sid, Project: "apollo", Limit: 1})
	if len(hits) != 1 || hits[0].Source.ArtifactID != text || hits[0].Source.Document != "notes.md" || hits[0].Source.By != core.DefaultPrincipal {
		t.Fatalf("hits=%+v", hits)
	}
}

// Acceptance: §Session and Run Lifecycle #7 – posting a message starts a run with it as input.
func TestPostMessageStartsARun(t *testing.T) {
	srv, orch, s := newTestServer(t)
	defer srv.Close()
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	post := func(body string, want int) map[string]any {
		t.Helper()
		resp, err := srv.Client().Post(srv.URL+"/v1/sessions/"+string(sid)+"/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != want {
			t.Fatalf("status %d want %d: %v", resp.StatusCode, want, out)
		}
		return out
	}
	out := post(`{"role":"user","content":"What is new?","run":{"workflow":{"stages":[{"name":"s","agents":[{"name":"echo","prompt":"Answer: {{input.message}}"}]}]}}}`, http.StatusCreated)
	runID, _ := out["run_id"].(string)
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if res, err := orch.Wait(wctx, core.DefaultTenant, core.RunID(runID)); err != nil || res.Status != core.RunSucceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	post(`{"role":"user","content":"x","run":{"template":"missing"}}`, http.StatusBadRequest)
	post(`{"role":"user","content":"x","run":{}}`, http.StatusBadRequest)
	post(`{"role":"user","content":"x","run":{"template":"t","input":{"message":"mine"}}}`, http.StatusBadRequest)
	humans := func() int {
		msgs, _ := s.ListMessages(ctx, core.DefaultTenant, sid)
		return len(slices.DeleteFunc(msgs, func(m store.MessageProjection) bool { return m.Role != "user" }))
	}
	if n := humans(); n != 1 {
		t.Fatalf("a run that could not start left %d messages, want 1", n)
	}
	first := post(`{"role":"user","content":"again","run":{"idempotency_key":"k","workflow":{"stages":[{"name":"s","agents":[{"name":"echo","prompt":"x"}]}]}}}`, http.StatusCreated)
	second := post(`{"role":"user","content":"again","run":{"idempotency_key":"k","workflow":{"stages":[{"name":"s","agents":[{"name":"echo","prompt":"x"}]}]}}}`, http.StatusCreated)
	if first["run_id"] != second["run_id"] || second["replayed"] != true || second["event"] != nil {
		t.Fatalf("a replayed post: first %v, second %v", first, second)
	}
	if n := humans(); n != 2 {
		t.Fatalf("a replayed post was stored again: %d messages", n)
	}
}
