package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/api"
	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
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
	return httptest.NewServer(srv.Handler()), orch, s
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

	buf := make([]byte, 4096)
	gotTerminal := false
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			if strings.Contains(chunk, "run_state_changed") && strings.Contains(chunk, "succeeded") {
				gotTerminal = true
			}
		}
		if err != nil {
			break
		}
		if gotTerminal {
			break
		}
	}
	if !gotTerminal {
		t.Fatal("did not see terminal succeeded event over SSE")
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
	if len(tools.Tools) != 3 || tools.Tools[0].Name == "" {
		t.Fatalf("unexpected tools: %+v", tools.Tools)
	}
}
