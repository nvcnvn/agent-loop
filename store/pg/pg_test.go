//go:build pg
// +build pg

// Integration tests for the pg driver.
//
// Run with a Postgres reachable via the AGENT_LOOP_PG_DSN env var:
//
//	AGENT_LOOP_PG_DSN=postgres://postgres:postgres@localhost:5432/agentloop?sslmode=disable \
//	  go test -tags=pg -count=1 ./store/pg/...
//
// The same set of acceptance scenarios as the in-memory store tests is
// exercised against the real driver to verify behavioural parity.
package pg_test

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nvcnvn/agent-loop/api"
	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/store/pg"
	"github.com/nvcnvn/agent-loop/tool"
)

func dsn(t *testing.T) string {
	v := os.Getenv("AGENT_LOOP_PG_DSN")
	if v == "" {
		t.Skip("AGENT_LOOP_PG_DSN not set")
	}
	return v
}

// freshStore returns a clean Store with the schema applied and any leftover
// data from prior tests truncated.
func freshStore(t *testing.T) *pg.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn(t))
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	s := pg.NewFromPool(pool)
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE messages, run_events, event_counters, approvals, artifacts, tasks, runs, sessions RESTART IDENTITY CASCADE`,
	); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestPg_SequenceMonotonicUnderConcurrency(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	sid, err := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	rid, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	const N = 50
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Append(ctx, []core.Event{{
				TenantID:   core.DefaultTenant,
				SessionID:  sid,
				RunID:      rid,
				Type:       core.EvSystem,
				Visibility: core.VisibilityInternal,
			}})
			if err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()

	events, err := s.ListRunEvents(ctx, core.DefaultTenant, rid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != N {
		t.Fatalf("got %d events, want %d", len(events), N)
	}
	seen := map[int64]bool{}
	for _, ev := range events {
		if ev.Sequence < 1 || ev.Sequence > int64(N) {
			t.Fatalf("seq out of range: %d", ev.Sequence)
		}
		if seen[ev.Sequence] {
			t.Fatalf("duplicate seq %d", ev.Sequence)
		}
		seen[ev.Sequence] = true
	}
}

func TestPg_RunIdempotency(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	r := store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunQueued, IdempotencyKey: "k1"}
	id1, err := s.CreateRun(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateRun(ctx, r)
	if err == nil {
		t.Fatal("expected conflict on duplicate idempotency key")
	}
	got, err := s.FindByIdempotency(ctx, core.DefaultTenant, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id1 {
		t.Fatalf("idempotency mismatch: %s vs %s", got.ID, id1)
	}
}

func TestPg_TenantIsolation(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	t1 := core.TenantID("11111111-1111-1111-1111-111111111111")
	t2 := core.TenantID("22222222-2222-2222-2222-222222222222")
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: t1, CreatedBy: core.DefaultPrincipal})
	if _, err := s.GetSession(ctx, t2, sid); err == nil {
		t.Fatal("expected cross-tenant read denied")
	}
}

func TestPg_ApprovalResolveRoundtrip(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	rid, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunRunning})
	id, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: rid, Reason: "x"})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan store.Approval, 1)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() {
		a, err := s.WaitForApproval(wctx, id)
		if err != nil {
			t.Errorf("wait: %v", err)
			return
		}
		done <- a
	}()

	time.Sleep(150 * time.Millisecond)
	if err := s.ResolveApproval(ctx, core.DefaultTenant, id, "approved", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if a.Status != "approved" {
			t.Fatalf("status=%s", a.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for approval")
	}
}

// End-to-end: drive the orchestrator + REST API using the pg driver.
func TestPg_EndToEndRunOverREST(t *testing.T) {
	s := freshStore(t)
	reg := tool.NewRegistry()
	_ = tool.RegisterBuiltins(reg)
	prov := model.NewMockProvider(model.MockResponse{Content: "hello pg"})
	orch := runtime.NewOrchestrator(s, reg, []runtime.Agent{
		{Name: "echo", Model: "mock", Provider: prov},
	})
	srv := httptest.NewServer(api.NewServer(orch, s).Handler())
	defer srv.Close()
	c := srv.Client()

	// Create session.
	resp, err := c.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{"title":"pg-demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sresp struct {
		SessionID core.SessionID `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sresp)

	// Start a run.
	body, _ := json.Marshal(map[string]any{
		"session_id": sresp.SessionID,
		"workflow": map[string]any{
			"stages": []map[string]any{{
				"name":   "s",
				"agents": []map[string]any{{"name": "echo", "prompt": "hi"}},
			}},
		},
	})
	resp2, err := c.Post(srv.URL+"/v1/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var rresp struct {
		RunID core.RunID `json:"run_id"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&rresp)

	// Wait via orchestrator.
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := orch.Wait(wctx, rresp.RunID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Status != core.RunSucceeded {
		t.Fatalf("status=%s reason=%s", res.Status, res.FailureReason)
	}

	// Strict replay must match the persisted terminal state.
	rep, err := runtime.Replay(wctx, s, core.DefaultTenant, rresp.RunID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if rep.Status != core.RunSucceeded {
		t.Fatalf("replay status=%s", rep.Status)
	}

	// Server returns the same status via GET.
	r, err := c.Get(srv.URL + "/v1/runs/" + string(rresp.RunID))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", r.StatusCode)
	}
}
