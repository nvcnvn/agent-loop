package api_test

import (
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

const (
	tenantA = core.TenantID("00000000-0000-0000-0000-0000000000aa")
	userA   = core.PrincipalID("00000000-0000-0000-0000-0000000000a1")
)

func TestParseAPIKeys(t *testing.T) {
	keys, err := api.ParseAPIKeys(string(tenantA) + ":" + string(userA) + ":s3cret:with:colons, " + string(core.DefaultTenant) + ":" + string(core.DefaultPrincipal) + ":other")
	if err != nil {
		t.Fatal(err)
	}
	if keys["s3cret:with:colons"] != (api.Identity{Tenant: tenantA, Principal: userA}) || len(keys) != 2 {
		t.Fatalf("keys=%v", keys)
	}
	for _, bad := range []string{"", "tenant:principal", "a:b:", ":b:secretkey", "a::secretkey", "a:b:secretkey,a:b:secretkey"} {
		_, err := api.ParseAPIKeys(bad)
		if err == nil {
			t.Fatalf("ParseAPIKeys(%q) accepted", bad)
		}
		if strings.Contains(err.Error(), "secretkey") {
			t.Fatalf("error leaks the key: %v", err)
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
