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
	"errors"
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
	"github.com/nvcnvn/agent-loop/store/storetest"
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
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE agents, memories, messages, run_events, event_counters, approvals, artifacts, tasks, runs, sessions RESTART IDENTITY CASCADE`,
	); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// Covers a database created before schema_migrations existed, replicas
// migrating at once, and a database a newer binary migrated further.
func TestPg_MigrateConcurrentUpgrade(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Migrate(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
	var applied, latest int
	if err := pool.QueryRow(ctx, `SELECT count(*), max(version) FROM schema_migrations`).Scan(&applied, &latest); err != nil {
		t.Fatal(err)
	}
	if applied != latest || latest < 1 {
		t.Fatalf("schema_migrations has %d rows, latest version %d", applied, latest)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES (9999, 'future')`); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = 9999`)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate against newer database: %v", err)
	}
}

func TestPg_Ping(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	s := pg.NewFromPool(pool)
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("ping open store: %v", err)
	}
	s.Close()
	if err := s.Ping(ctx); err == nil {
		t.Fatal("ping after Close succeeded")
	}
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
	r := store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunQueued, IdempotencyKey: "k1", StartedBy: core.DefaultPrincipal}
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
	if got.ID != id1 || got.StartedBy != core.DefaultPrincipal {
		t.Fatalf("idempotency mismatch: %s vs %s, started by %q", got.ID, id1, got.StartedBy)
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

func TestPg_Memories(t *testing.T) {
	s := freshStore(t)
	session := func() core.SessionID {
		sid, err := s.CreateSession(context.Background(), store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
		if err != nil {
			t.Fatal(err)
		}
		return sid
	}
	storetest.Memories(t, s, session(), session())
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
	if err := s.ResolveApproval(ctx, core.DefaultTenant, id, "approved", "", json.RawMessage(`{"ok":true}`)); err != nil {
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

func TestPg_UpdateRunStatusAndUsage(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	rid, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunQueued})
	if err := s.UpdateRunStatus(ctx, core.DefaultTenant, rid, core.RunRunning, ""); err != nil {
		t.Fatal(err)
	}
	want := core.Usage{ModelCalls: 2, ToolCalls: 3, PromptTokens: 40}
	if err := s.UpdateRunUsage(ctx, core.DefaultTenant, rid, want); err != nil {
		t.Fatal(err)
	}
	run, err := s.GetRun(ctx, core.DefaultTenant, rid)
	if err != nil || run.Usage != want || run.Status != core.RunRunning {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if err := s.UpdateRunUsage(ctx, "00000000-0000-0000-0000-0000000000bb", rid, want); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cross-tenant update err=%v", err)
	}
	if err := s.UpdateRunStatus(ctx, core.DefaultTenant, rid, core.RunAwaitingApproval, "gate"); err != nil {
		t.Fatal(err)
	}
	run, err = s.GetRun(ctx, core.DefaultTenant, rid)
	if err != nil || run.Usage != want || run.Status != core.RunAwaitingApproval || run.FailureReason != "gate" || run.StartedAt.IsZero() {
		t.Fatalf("after status update run=%+v err=%v", run, err)
	}
}

func TestPg_ListUnfinishedRuns(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	want := map[core.RunID]bool{}
	for _, status := range []core.RunStatus{core.RunQueued, core.RunRunning, core.RunAwaitingApproval, core.RunSucceeded, core.RunFailed, core.RunCanceled, core.RunTimedOut} {
		id, err := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		want[id] = !status.Terminal()
	}
	runs, err := s.ListUnfinishedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d unfinished runs want 3", len(runs))
	}
	for _, r := range runs {
		if !want[r.ID] {
			t.Fatalf("finished run %s (%s) listed", r.ID, r.Status)
		}
	}
}

func TestPg_CancelRunApprovals(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	rid, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid, Status: core.RunAwaitingApproval})
	open, _ := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: rid})
	done, _ := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: rid})
	if err := s.ResolveApproval(ctx, core.DefaultTenant, done, "approved", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRunApprovals(ctx, core.DefaultTenant, rid); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[core.ApprovalID]string{open: "canceled", done: "approved"} {
		if a, err := s.GetApproval(ctx, core.DefaultTenant, id); err != nil || a.Status != want {
			t.Fatalf("approval %s=%+v err=%v want %s", id, a, err, want)
		}
	}
}

func TestPg_ExpiredApprovalCannotBeApproved(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	expires := time.Now().Add(-time.Second).UTC().Truncate(time.Microsecond)
	id, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, id, "approved", "", nil); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("approve after expiry err=%v want ErrConflict", err)
	}
	if err := s.ResolveApproval(ctx, core.DefaultTenant, id, "expired", "", nil); err != nil {
		t.Fatalf("expire: %v", err)
	}
	a, err := s.GetApproval(ctx, core.DefaultTenant, id)
	if err != nil || a.Status != "expired" || !a.ExpiresAt.Equal(expires) {
		t.Fatalf("approval=%+v err=%v", a, err)
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
	res, err := orch.Wait(wctx, core.DefaultTenant, rresp.RunID)
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

	// Re-execution reads the stored requests and responses back out of Postgres.
	again, err := orch.ReExecute(wctx, core.DefaultTenant, rresp.RunID, core.SystemPrincipal)
	if err != nil || again.Outputs["s"] != "hello pg" {
		t.Fatalf("re-execute: %+v err=%v", again, err)
	}
	if copyRun, err := s.GetRun(wctx, core.DefaultTenant, again.RunID); err != nil || copyRun.ParentRunID != rresp.RunID {
		t.Fatalf("copy parent=%q err=%v", copyRun.ParentRunID, err)
	}
}

func TestPg_Approvals(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	run, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid})
	other, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid})
	storetest.Approvals(t, s, run, other)
}

func TestPg_Artifacts(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	run, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid})
	storetest.Artifacts(t, s, sid, run)
}

func TestPg_Participants(t *testing.T) {
	storetest.Participants(t, freshStore(t))
}

// Acceptance: §SSE – events and approval decisions are pushed, not polled, and survive a lost
// LISTEN connection.
func TestPg_PushesEventsAndApprovals(t *testing.T) {
	s := freshStore(t)
	s.Recheck = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sid, _ := s.CreateSession(ctx, store.Session{TenantID: core.DefaultTenant, CreatedBy: core.DefaultPrincipal})
	rid, _ := s.CreateRun(ctx, store.Run{TenantID: core.DefaultTenant, SessionID: sid})
	events, stop := s.Subscribe(ctx, rid, 0)
	defer stop()
	appendOne := func() {
		t.Helper()
		if _, err := s.Append(ctx, []core.Event{{TenantID: core.DefaultTenant, SessionID: sid, RunID: rid, Type: core.EvSystem,
			Visibility: core.VisibilityInternal, ActorID: core.SystemPrincipal}}); err != nil {
			t.Fatal(err)
		}
	}
	receive := func(seq int64) {
		t.Helper()
		select {
		case ev := <-events:
			if ev.Sequence != seq || ev.ActorID != core.SystemPrincipal {
				t.Fatalf("got event %d by %q, want %d by the system", ev.Sequence, ev.ActorID, seq)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d was not pushed", seq)
		}
	}
	appendOne()
	receive(1)

	admin, err := pgxpool.New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var killed int
	for killed == 0 && ctx.Err() == nil {
		if err := admin.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND datname = current_database() AND query LIKE 'LISTEN %'`).Scan(&killed); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	appendOne()
	receive(2)

	aid, err := s.CreateApproval(ctx, store.Approval{TenantID: core.DefaultTenant, RunID: rid, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	resolved := make(chan store.Approval, 1)
	go func() {
		a, err := s.WaitForApproval(ctx, aid)
		if err != nil {
			t.Error(err)
		}
		resolved <- a
	}()
	time.Sleep(100 * time.Millisecond)
	if err := s.ResolveApproval(ctx, core.DefaultTenant, aid, "approved", core.DefaultPrincipal, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-resolved:
		if a.Status != "approved" {
			t.Fatalf("approval %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval decision was not pushed")
	}
}

func TestPg_Agents(t *testing.T) {
	storetest.Agents(t, freshStore(t))
}

func TestPg_Retention(t *testing.T) {
	storetest.Retention(t, freshStore(t))
}

// Writing into a session a purge deleted fails instead of leaving orphans.
func TestPg_NoWritesIntoAPurgedSession(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	gone := core.SessionID(core.NewID())
	if _, err := s.Append(ctx, []core.Event{{TenantID: core.DefaultTenant, SessionID: gone, Type: core.EvSystem, Visibility: core.VisibilityInternal}}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("append: %v", err)
	}
	if _, err := s.PutMemory(ctx, store.Memory{TenantID: core.DefaultTenant, Scope: store.MemorySession, SessionID: gone, Content: "x", Embedding: []float32{1}, EmbedModel: "m"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("session memory: %v", err)
	}
}
