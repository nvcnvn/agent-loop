package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nvcnvn/agent-loop/api"
	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

const (
	tenantA = core.TenantID("00000000-0000-0000-0000-0000000000aa")
	userA   = core.PrincipalID("00000000-0000-0000-0000-0000000000a1")
)

func TestParseAPIKeys(t *testing.T) {
	keys, err := api.ParseAPIKeys(string(tenantA) + ":" + string(userA) + ":s3cret:with:colons, operator:" + string(core.DefaultTenant) + ":" + string(core.DefaultPrincipal) + ":admin:key")
	if err != nil {
		t.Fatal(err)
	}
	if keys["s3cret:with:colons"] != (api.Identity{Tenant: tenantA, Principal: userA, Role: policy.RoleMember}) ||
		keys["admin:key"] != (api.Identity{Tenant: core.DefaultTenant, Principal: core.DefaultPrincipal, Role: policy.RoleOperator}) || len(keys) != 2 {
		t.Fatalf("keys=%v", keys)
	}
	for _, bad := range []string{"", "tenant:principal", "a:b:", ":b:secretkey", "a::secretkey", "a:b:secretkey,a:b:secretkey", string(tenantA) + ":bot:secretkey", "root:" + string(tenantA) + ":" + string(userA) + ":secretkey"} {
		_, err := api.ParseAPIKeys(bad)
		if err == nil {
			t.Fatalf("ParseAPIKeys(%q) accepted", bad)
		}
		if strings.Contains(err.Error(), "secretkey") {
			t.Fatalf("error leaks the key: %v", err)
		}
	}
}

func TestHeaderIdentityNeedsUUIDs(t *testing.T) {
	for h, ok := range map[string]bool{"": true, string(userA): true, "operator-3": false} {
		r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		r.Header.Set("X-Principal-ID", h)
		if _, err := api.HeaderIdentity(r); (err == nil) != ok {
			t.Errorf("X-Principal-ID %q: err=%v", h, err)
		}
	}
}

// Acceptance: §Security #5 – API key authentication.
func TestAPIKeyAuthentication(t *testing.T) {
	s := store.NewMemStore()
	prov := model.NewMockProvider(model.MockResponse{Content: "hi"})
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "echo", Model: "m", Provider: prov}})
	srv := api.NewServer(orch, s)
	srv.Auth = api.APIKeys(map[string]api.Identity{"s3cret": {Tenant: tenantA, Principal: userA}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	do := func(method, path, key string, header map[string]string, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for _, path := range []string{"/v1/sessions", "/v1/", "/nope"} {
		for _, key := range []string{"", "wrong"} {
			resp := do(http.MethodGet, path, key, map[string]string{"X-Tenant-ID": string(tenantA)}, "")
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
				t.Fatalf("GET %s key %q: status=%d", path, key, resp.StatusCode)
			}
		}
	}
	resp := do(http.MethodGet, "/v1/sessions", "", map[string]string{"Authorization": "bearer s3cret"}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lower-case bearer scheme: status=%d", resp.StatusCode)
	}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		resp := do(http.MethodGet, path, "", nil, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d without a key", path, resp.StatusCode)
		}
	}

	// The key decides the identity; identity headers are ignored.
	resp = do(http.MethodPost, "/v1/sessions", "s3cret", map[string]string{"X-Tenant-ID": string(core.DefaultTenant), "X-Principal-ID": "spoofed"}, `{}`)
	var created struct {
		SessionID core.SessionID `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sess, err := s.GetSession(context.Background(), tenantA, created.SessionID)
	if err != nil || sess.CreatedBy != userA {
		t.Fatalf("session=%+v err=%v; want it in the key's tenant, created by the key's principal", sess, err)
	}

	runID, err := orch.StartRun(context.Background(), runtime.StartRunInput{TenantID: tenantA, SessionID: created.SessionID, Workflow: runtime.Workflow{
		Stages: []runtime.Stage{{Name: "s", Agents: []runtime.AgentStep{{Name: "echo", Prompt: "x"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := orch.Wait(ctx, tenantA, runID); err != nil {
		t.Fatal(err)
	}
	// EventSource cannot set headers, so the stream also takes the key as access_token.
	resp = do(http.MethodGet, "/v1/runs/"+string(runID)+"/stream?access_token=s3cret", "", nil, "")
	stream, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(stream), `"status":"succeeded"`) {
		t.Fatalf("stream status=%d body=%s", resp.StatusCode, stream)
	}
	resp = do(http.MethodGet, "/v1/runs/"+string(runID)+"?access_token=s3cret", "", nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("access_token outside the stream: status=%d want 401", resp.StatusCode)
	}
}

func FuzzParseAPIKeys(f *testing.F) {
	f.Add("00000000-0000-0000-0000-000000000001:00000000-0000-0000-0000-000000000002:k1")
	f.Add("a:b:c,,")
	f.Add("00000000-0000-0000-0000-000000000001:00000000-0000-0000-0000-000000000002:k:with:colons")
	f.Fuzz(func(t *testing.T, spec string) {
		keys, err := api.ParseAPIKeys(spec)
		if err != nil {
			return
		}
		for key, id := range keys {
			if key == "" || uuid.Validate(string(id.Tenant)) != nil || uuid.Validate(string(id.Principal)) != nil {
				t.Fatalf("accepted %q as %+v", key, id)
			}
		}
	})
}

// Acceptance: §Security #6 – roles and session membership.
func TestRolesAndSessionMembership(t *testing.T) {
	s := store.NewMemStore()
	orch := runtime.NewOrchestrator(s, tool.NewRegistry(), []runtime.Agent{{Name: "echo", Model: "m", Provider: model.NewMockProvider()}})
	srv := api.NewServer(orch, s)
	srv.Debug = true
	principal := func(n int) core.PrincipalID {
		return core.PrincipalID(fmt.Sprintf("00000000-0000-0000-0000-0000000001%02d", n))
	}
	srv.Auth = api.APIKeys(map[string]api.Identity{
		"owner":    {Tenant: tenantA, Principal: principal(1), Role: policy.RoleMember},
		"member":   {Tenant: tenantA, Principal: principal(2), Role: policy.RoleMember},
		"viewer":   {Tenant: tenantA, Principal: principal(3), Role: policy.RoleViewer},
		"operator": {Tenant: tenantA, Principal: principal(4), Role: policy.RoleOperator},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	call := func(key, method, path, body string, want int) []byte {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s %s: status %d want %d: %s", key, method, path, resp.StatusCode, want, out)
		}
		return out
	}
	var created struct {
		SessionID core.SessionID `json:"session_id"`
	}
	_ = json.Unmarshal(call("owner", http.MethodPost, "/v1/sessions", `{}`, http.StatusCreated), &created)
	sess := "/v1/sessions/" + string(created.SessionID)
	call("viewer", http.MethodPost, "/v1/sessions", `{}`, http.StatusForbidden)

	call("operator", http.MethodGet, sess, "", http.StatusOK)
	call("member", http.MethodGet, sess, "", http.StatusNotFound)
	call("member", http.MethodPost, sess+"/messages", `{"role":"user","content":"hi"}`, http.StatusNotFound)
	if out := call("member", http.MethodGet, "/v1/sessions", "", http.StatusOK); strings.Contains(string(out), string(created.SessionID)) {
		t.Fatalf("a non-participant lists the session: %s", out)
	}

	call("owner", http.MethodPut, sess+"/participants/"+string(principal(2)), `{"role":"viewer"}`, http.StatusOK)
	call("owner", http.MethodPut, sess+"/participants/"+string(principal(3)), `{"role":"member"}`, http.StatusOK)
	call("owner", http.MethodPut, sess+"/participants/"+string(principal(3)), `{"role":"boss"}`, http.StatusBadRequest)
	call("member", http.MethodGet, sess+"/messages", "", http.StatusOK)
	call("member", http.MethodPost, sess+"/messages", `{"role":"user","content":"hi"}`, http.StatusForbidden)
	call("member", http.MethodPut, sess+"/participants/"+string(principal(2)), `{"role":"owner"}`, http.StatusForbidden)
	call("viewer", http.MethodPost, sess+"/messages", `{"role":"user","content":"hi"}`, http.StatusForbidden)
	if out := call("member", http.MethodGet, "/v1/sessions", "", http.StatusOK); !strings.Contains(string(out), string(created.SessionID)) {
		t.Fatalf("a participant does not list the session: %s", out)
	}

	call("owner", http.MethodPut, sess+"/participants/"+string(principal(2)), `{"role":"member"}`, http.StatusOK)
	call("member", http.MethodPost, sess+"/messages", `{"role":"user","content":"hi"}`, http.StatusCreated)
	if out := call("viewer", http.MethodGet, sess+"/participants", "", http.StatusOK); strings.Count(string(out), `"principal_id"`) != 3 {
		t.Fatalf("participants: %s", out)
	}
	call("operator", http.MethodDelete, sess+"/participants/"+string(principal(2)), "", http.StatusNoContent)
	call("member", http.MethodGet, sess, "", http.StatusNotFound)
	call("owner", http.MethodDelete, sess+"/participants/"+string(principal(2)), "", http.StatusNotFound)

	notFound := string(call("member", http.MethodGet, "/v1/sessions/"+core.NewID(), "", http.StatusNotFound))
	if hidden := string(call("member", http.MethodGet, sess, "", http.StatusNotFound)); hidden != notFound {
		t.Fatalf("a session the caller may not see answers %s, a missing one %s", hidden, notFound)
	}
	call("member", http.MethodGet, "/v1/runs/not-a-uuid", "", http.StatusNotFound)

	run := `{"session_id":"` + string(created.SessionID) + `","idempotency_key":"k","workflow":{"stages":[{"name":"s","agents":[{"name":"echo","prompt":"x"}]}]}}`
	call("owner", http.MethodPost, "/v1/runs", run, http.StatusAccepted)
	var mine struct {
		SessionID core.SessionID `json:"session_id"`
	}
	_ = json.Unmarshal(call("member", http.MethodPost, "/v1/sessions", `{}`, http.StatusCreated), &mine)
	call("member", http.MethodPost, "/v1/runs", strings.Replace(run, string(created.SessionID), string(mine.SessionID), 1), http.StatusConflict)

	call("member", http.MethodGet, "/v1/debug/agents", "", http.StatusForbidden)
	call("operator", http.MethodGet, "/v1/debug/agents", "", http.StatusOK)
}

func TestAgentsOverREST(t *testing.T) {
	s := store.NewMemStore()
	reg := tool.NewRegistry()
	_ = tool.RegisterBuiltins(reg)
	orch := runtime.NewOrchestrator(s, reg, []runtime.Agent{{Name: "echo", Instructions: "secret plan", Provider: model.NewMockProvider()}})
	orch.Provider = model.NewMockProvider()
	srv := api.NewServer(orch, s)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	call := func(method, path, role, body string, want int) string {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("X-Role", role)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s as %s: %d want %d: %s", method, path, role, resp.StatusCode, want, out)
		}
		return string(out)
	}
	call(http.MethodPut, "/v1/agents/writer", "member", `{"instructions":"x"}`, http.StatusForbidden)
	if out := call(http.MethodPut, "/v1/agents/writer", "admin", `{"instructions":"Write well.","tools":["echo"]}`, http.StatusOK); !strings.Contains(out, `"version":1`) {
		t.Fatalf("put: %s", out)
	}
	call(http.MethodPut, "/v1/agents/writer", "admin", `{"tools":["nope"]}`, http.StatusBadRequest)
	call(http.MethodPut, "/v1/agents/echo", "admin", `{}`, http.StatusConflict)
	if out := call(http.MethodGet, "/v1/agents", "member", "", http.StatusOK); strings.Contains(out, "secret plan") || strings.Contains(out, "Write well.") || !strings.Contains(out, `"writer"`) {
		t.Fatalf("a member's list: %s", out)
	}
	if out := call(http.MethodGet, "/v1/agents", "operator", "", http.StatusOK); !strings.Contains(out, "Write well.") || strings.Contains(out, "secret plan") {
		t.Fatalf("an operator's list: %s", out)
	}
	call(http.MethodPut, "/v1/agents/critic", "admin", `{"instruction":"typo"}`, http.StatusBadRequest)
	call(http.MethodDelete, "/v1/agents/writer", "admin", "", http.StatusNoContent)
	call(http.MethodDelete, "/v1/agents/writer", "admin", "", http.StatusNotFound)
	call(http.MethodDelete, "/v1/agents/echo", "admin", "", http.StatusConflict)
}
