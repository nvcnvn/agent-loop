// Package api exposes the agent-loop runtime over HTTP REST + SSE.
//
// The handler set covers the first-slice contract from
// docs/04-contracts-and-tests.md:
//
//	POST   /v1/sessions
//	GET    /v1/sessions/{sid}
//	POST   /v1/sessions/{sid}/messages
//	POST   /v1/runs
//	GET    /v1/runs/{rid}
//	GET    /v1/runs/{rid}/events?after=<seq>
//	GET    /v1/runs/{rid}/stream     (SSE; honours Last-Event-ID; ?deltas=1 adds reply deltas)
//	POST   /v1/runs/{rid}/cancel
//	POST   /v1/runs/{rid}/resume
//	POST   /v1/runs/{rid}/reexecute  (replays recorded model and tool outputs; waits)
//	GET    /v1/approvals?run_id=&status=&after=&limit=  (default status: requested; "all" for every status)
//	POST   /v1/approvals/{aid}
//	GET    /healthz                  (liveness)
//	GET    /readyz                   (readiness: the store answers a ping)
//	GET    /metrics                  (Prometheus text format)
//
// With Server.Debug set it also serves the debug console at / and /ui/, and:
//
//	GET    /v1/debug/agents
//	GET    /v1/debug/tools
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/observe"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
)

//go:embed web/*
var webFS embed.FS

// Server wires the runtime into an http.Handler.
type Server struct {
	Orch  *runtime.Orchestrator
	Store store.Store
	// Auth resolves who a request acts as; nil means the default tenant and principal.
	Auth Authenticator
	// Debug serves the debug console and /v1/debug/*, which expose agent instructions.
	Debug bool
	// MaxBodyBytes caps request bodies (zero means no cap); larger ones get 413.
	MaxBodyBytes int64
	// Log receives request and error logs; NewServer sets slog.Default().
	Log *slog.Logger
	// Metrics, when set, is served at /metrics and records HTTP requests; NewServer uses Orch.Metrics.
	Metrics *observe.Metrics

	streamsClosed chan struct{}
	closeStreams  sync.Once
}

func NewServer(orch *runtime.Orchestrator, s store.Store) *Server {
	return &Server{Orch: orch, Store: s, Auth: HeaderIdentity, MaxBodyBytes: 1 << 20, Log: slog.Default(), Metrics: orch.Metrics, streamsClosed: make(chan struct{})}
}

// HTTPServer returns an http.Server for Handler with read and idle timeouts.
func (s *Server) HTTPServer(addr string) *http.Server {
	if s.streamsClosed == nil {
		s.streamsClosed = make(chan struct{})
	}
	// No WriteTimeout: SSE streams stay open for the whole run.
	hs := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// Shutdown waits for active requests, which an open SSE stream never stops being.
	hs.RegisterOnShutdown(func() { s.closeStreams.Do(func() { close(s.streamsClosed) }) })
	return hs
}

// Handler returns an http.Handler with all v1 routes wired.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.Debug {
		assets, err := fs.Sub(webFS, "web")
		if err != nil {
			panic(err)
		}
		mux.Handle("GET /ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(assets))))
		mux.HandleFunc("GET /", s.debugConsole)
		mux.HandleFunc("GET /v1/debug/agents", s.listAgents)
		mux.HandleFunc("GET /v1/debug/tools", s.listTools)
	}
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{sid}", s.getSession)
	mux.HandleFunc("GET /v1/sessions/{sid}/messages", s.listMessages)
	mux.HandleFunc("GET /v1/sessions/{sid}/runs", s.listRuns)
	mux.HandleFunc("POST /v1/sessions/{sid}/messages", s.postMessage)
	mux.HandleFunc("POST /v1/runs", s.startRun)
	mux.HandleFunc("GET /v1/runs/{rid}", s.getRun)
	mux.HandleFunc("GET /v1/runs/{rid}/events", s.listEvents)
	mux.HandleFunc(streamRoute, s.streamEvents)
	mux.HandleFunc("POST /v1/runs/{rid}/cancel", s.cancelRun)
	mux.HandleFunc("POST /v1/runs/{rid}/resume", s.resumeRun)
	mux.HandleFunc("POST /v1/runs/{rid}/reexecute", s.reExecuteRun)
	mux.HandleFunc("GET /v1/approvals", s.listApprovals)
	mux.HandleFunc("POST /v1/approvals/{aid}", s.resolveApproval)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	if s.Metrics != nil {
		mux.Handle("GET /metrics", s.Metrics)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.MaxBodyBytes > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, s.MaxBodyBytes)
		}
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		_, r.Pattern = mux.Handler(r)
		route := r.Pattern
		id, err := s.resolve(r)
		if err != nil {
			rec.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(rec, http.StatusUnauthorized, "unauthorized", err.Error())
		} else {
			r = withIdentity(r, id)
			mux.ServeHTTP(rec, r)
		}
		if route == "" {
			route = "unmatched"
		}
		s.Metrics.Observe("agentloop_http_request_duration_seconds", time.Since(started).Seconds(),
			"route", route, "code", strconv.Itoa(rec.status))
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(started)}
		if err == nil {
			attrs = append(attrs, "tenant_id", id.Tenant)
		}
		s.Log.InfoContext(r.Context(), "http request", attrs...)
	})
}

// publicRoutes need no identity; every other route, matched or not, does.
var publicRoutes = map[string]bool{
	"GET /healthz": true,
	"GET /readyz":  true,
	"GET /metrics": true,
	"GET /":        true,
	"GET /ui/":     true,
}

// resolve authenticates a request whose r.Pattern is the route it will match.
func (s *Server) resolve(r *http.Request) (Identity, error) {
	if s.Auth == nil || publicRoutes[r.Pattern] {
		return identityOf(r), nil
	}
	return s.Auth(r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		s.Log.ErrorContext(ctx, "readiness ping failed", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "not_ready", "store unreachable")
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) debugConsole(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, webFS, "web/index.html")
}

// ---- request/response shapes ----

type createSessionReq struct {
	Title string `json:"title"`
}
type createSessionResp struct {
	SessionID core.SessionID `json:"session_id"`
}

type postMessageReq struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type startRunReq struct {
	SessionID      core.SessionID         `json:"session_id"`
	IdempotencyKey string                 `json:"idempotency_key,omitempty"`
	Workflow       runtime.Workflow       `json:"workflow"`
	Template       string                 `json:"template,omitempty"`
	Input          map[string]string      `json:"input,omitempty"`
	Images         map[string]model.Image `json:"images,omitempty"`
	Limits         core.Limits            `json:"limits,omitempty"`
}
type startRunResp struct {
	RunID core.RunID `json:"run_id"`
}

type errorResp struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Retryable   bool   `json:"retryable"`
	Correlation string `json:"correlation_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResp{Code: code, Message: msg})
}

func writeDecodeErr(w http.ResponseWriter, err error) {
	if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
		writeErr(w, http.StatusRequestEntityTooLarge, "validation_error", err.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
}

func tenantOf(r *http.Request) core.TenantID { return identityOf(r).Tenant }

func principalOf(r *http.Request) core.PrincipalID { return identityOf(r).Principal }

// ---- handlers ----

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeDecodeErr(w, err)
		return
	}
	id, err := s.Store.CreateSession(r.Context(), store.Session{
		TenantID:  tenantOf(r),
		CreatedBy: principalOf(r),
		Title:     req.Title,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, createSessionResp{SessionID: id})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sid := core.SessionID(r.PathValue("sid"))
	sess, err := s.Store.GetSession(r.Context(), tenantOf(r), sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.Store.ListSessions(r.Context(), tenantOf(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	sid := core.SessionID(r.PathValue("sid"))
	messages, err := s.Store.ListMessages(r.Context(), tenantOf(r), sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	messages = slices.DeleteFunc(messages, func(m store.MessageProjection) bool { return !externallyVisible(m.Visibility) })
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	sid := core.SessionID(r.PathValue("sid"))
	if _, err := s.Store.GetSession(r.Context(), tenantOf(r), sid); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	runs, err := s.Store.ListRuns(r.Context(), tenantOf(r), sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	var req postMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	sid := core.SessionID(r.PathValue("sid"))
	tenant := tenantOf(r)
	if _, err := s.Store.GetSession(r.Context(), tenant, sid); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	ev := core.Event{
		TenantID:   tenant,
		SessionID:  sid,
		Type:       core.EvHumanMessage,
		Visibility: core.VisibilityParticipant,
		ActorID:    principalOf(r),
		Payload:    mustJSON(map[string]any{"role": req.Role, "content": req.Content}),
	}
	out, err := s.Store.Append(r.Context(), []core.Event{ev})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if len(out) == 1 {
		if err := s.Store.AddMessage(r.Context(), store.MessageProjection{
			SessionID:  sid,
			EventID:    out[0].ID,
			Role:       req.Role,
			Visibility: core.VisibilityParticipant,
			Content:    req.Content,
		}); err != nil {
			s.Log.ErrorContext(r.Context(), "add message failed", "session_id", sid, "err", err)
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event": out[0]})
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var req startRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if req.SessionID == "" {
		writeErr(w, http.StatusBadRequest, "validation_error", "session_id required")
		return
	}
	if len(req.Workflow.Stages) == 0 && req.Template == "" {
		writeErr(w, http.StatusBadRequest, "validation_error", "workflow.stages or template required")
		return
	}
	idemFromHeader := r.Header.Get("Idempotency-Key")
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = idemFromHeader
	}
	runID, err := s.Orch.StartRun(r.Context(), runtime.StartRunInput{
		TenantID:       tenantOf(r),
		PrincipalID:    principalOf(r),
		SessionID:      req.SessionID,
		IdempotencyKey: req.IdempotencyKey,
		Workflow:       req.Workflow,
		Template:       req.Template,
		Input:          req.Input,
		Images:         req.Images,
		Limits:         req.Limits,
	})
	if errors.Is(err, core.ErrOverloaded) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "overloaded", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, startRunResp{RunID: runID})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	rid := core.RunID(r.PathValue("rid"))
	run, err := s.Store.GetRun(r.Context(), tenantOf(r), rid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	rid := core.RunID(r.PathValue("rid"))
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.Store.ListRunEvents(r.Context(), tenantOf(r), rid, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// Strip hidden_reasoning for the public-style endpoint; clients with
	// internal scope would use a different handler.
	visible := events[:0]
	for _, e := range events {
		if !externallyVisible(e.Visibility) {
			continue
		}
		visible = append(visible, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": visible})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	rid := core.RunID(r.PathValue("rid"))
	switch err := s.Orch.Cancel(r.Context(), tenantOf(r), rid, "api request"); {
	case errors.Is(err, core.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// reExecuteRun answers with the copy's result, whatever its status; an error
// means the copy could not start.
func (s *Server) reExecuteRun(w http.ResponseWriter, r *http.Request) {
	res, err := s.Orch.ReExecute(r.Context(), tenantOf(r), core.RunID(r.PathValue("rid")))
	switch {
	case res.RunID != "":
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, core.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, runtime.ErrNotRecorded):
		writeErr(w, http.StatusUnprocessableEntity, "not_recorded", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	rid := core.RunID(r.PathValue("rid"))
	switch err := s.Orch.Resume(r.Context(), tenantOf(r), rid); {
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, core.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, core.ErrOverloaded):
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "overloaded", err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// approvalPage is the default and largest page of GET /v1/approvals.
const approvalPage = 100

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ApprovalFilter{RunID: core.RunID(q.Get("run_id")), Status: q.Get("status"), After: core.ApprovalID(q.Get("after")), Limit: approvalPage}
	switch f.Status {
	case "":
		f.Status = "requested"
	case "all":
		f.Status = ""
	case "requested", "approved", "rejected", "expired", "canceled":
	default:
		writeErr(w, http.StatusBadRequest, "validation_error", "status must be requested, approved, rejected, expired, canceled or all")
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > approvalPage {
			writeErr(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("limit must be from 1 to %d", approvalPage))
			return
		}
		f.Limit = n
	}
	if f.After != "" && uuid.Validate(string(f.After)) != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "after must be an approval ID")
		return
	}
	if f.RunID != "" {
		if _, err := s.Store.GetRun(r.Context(), tenantOf(r), f.RunID); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
	}
	approvals, err := s.Store.ListApprovals(r.Context(), tenantOf(r), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	resp := map[string]any{"approvals": approvals}
	if len(approvals) == f.Limit {
		resp["next_after"] = approvals[len(approvals)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	aid := core.ApprovalID(r.PathValue("aid"))
	var req struct {
		Decision string          `json:"decision"`
		Reason   string          `json:"reason"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if req.Decision != "approved" && req.Decision != "rejected" {
		writeErr(w, http.StatusBadRequest, "validation_error", "decision must be approved|rejected")
		return
	}
	switch err := s.Store.ResolveApproval(r.Context(), tenantOf(r), aid, req.Decision, req.Payload); {
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	case errors.Is(err, core.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", "approval is already resolved or expired")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) listAgents(w http.ResponseWriter, _ *http.Request) {
	type agentDebug struct {
		Name         string   `json:"name"`
		Role         string   `json:"role,omitempty"`
		Instructions string   `json:"instructions,omitempty"`
		Model        string   `json:"model"`
		Tools        []string `json:"tools,omitempty"`
		Children     []string `json:"children,omitempty"`
		MaxToolLoops int      `json:"max_tool_loops"`
	}
	agents := make([]agentDebug, 0, len(s.Orch.Agents))
	for _, agent := range s.Orch.Agents {
		agents = append(agents, agentDebug{
			Name:         agent.Name,
			Role:         agent.Role,
			Instructions: agent.Instructions,
			Model:        agent.Model,
			Tools:        append([]string(nil), agent.Tools...),
			Children:     append([]string(nil), agent.Children...),
			MaxToolLoops: agent.MaxToolLoops,
		})
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) listTools(w http.ResponseWriter, _ *http.Request) {
	tools := s.Orch.Tools.List()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

// streamEvents serves SSE. It honours Last-Event-ID by parsing the last seen
// run-sequence number out of the value (we encode events as
// "<run-sequence>" so resume is trivial).
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	rid := core.RunID(r.PathValue("rid"))
	tenant := tenantOf(r)
	if _, err := s.Store.GetRun(r.Context(), tenant, rid); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	var deltas <-chan runtime.Delta
	if r.URL.Query().Get("deltas") == "1" {
		var stop func()
		deltas, stop = s.Orch.SubscribeDeltas(rid)
		defer stop()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var afterSeq int64
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if v, err := strconv.ParseInt(last, 10, 64); err == nil {
			afterSeq = v
		}
	} else if v, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); err == nil {
		afterSeq = v
	}

	// 1. Drain history first. A terminal event followed by others ended an
	// attempt of a run that was resumed later.
	history, _ := s.Store.ListRunEvents(r.Context(), tenant, rid, afterSeq, 0)
	for i, ev := range history {
		writeSSE(w, ev)
		afterSeq = ev.Sequence
		if endsRun(ev) && i == len(history)-1 {
			flusher.Flush()
			return
		}
	}
	flusher.Flush()

	// 2. Subscribe to live events.
	ch, cancel := s.Store.Subscribe(r.Context(), rid, afterSeq)
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.streamsClosed:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Sequence <= afterSeq {
				continue
			}
			for drained := false; !drained; {
				select {
				case d := <-deltas:
					writeDelta(w, d)
				default:
					drained = true
				}
			}
			writeSSE(w, ev)
			afterSeq = ev.Sequence
			flusher.Flush()
			if endsRun(ev) {
				return
			}
		case d := <-deltas:
			writeDelta(w, d)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// deltaEvent is the SSE event of a reply delta. It has no id, so it does not
// move Last-Event-ID; a reconnecting client misses deltas, not events.
const deltaEvent = "agent_message_delta"

func writeDelta(w http.ResponseWriter, d runtime.Delta) {
	body, _ := json.Marshal(d)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", deltaEvent, body)
}

func endsRun(ev core.Event) bool {
	if ev.Type != core.EvRunStateChanged {
		return false
	}
	var p struct {
		Status core.RunStatus `json:"status"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	return p.Status.Terminal()
}

func writeSSE(w http.ResponseWriter, ev core.Event) {
	if !externallyVisible(ev.Visibility) {
		// Hidden + audit events stay out of public SSE per requirements §5.2.
		return
	}
	envelope := map[string]any{
		"schema_version": "2026-05-09",
		"tenant_id":      ev.TenantID,
		"session_id":     ev.SessionID,
		"run_id":         ev.RunID,
		"sequence":       ev.Sequence,
		"visibility":     ev.Visibility,
		"occurred_at":    ev.OccurredAt,
		"payload":        ev.Payload,
		"event_id":       ev.ID,
		"correlation_id": ev.CorrelationID,
	}
	if ev.ParentEventID != "" {
		envelope["parent_event_id"] = ev.ParentEventID
	}
	if ev.CausationID != "" {
		envelope["causation_id"] = ev.CausationID
	}
	body, _ := json.Marshal(envelope)
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Sequence, ev.Type, body)
}

func externallyVisible(v core.Visibility) bool {
	switch v {
	case core.VisibilityHiddenReasoning, core.VisibilityAudit, core.VisibilityInternal:
		return false
	default:
		return true
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// ContextWithTimeout is exported as a small convenience used by examples.
func ContextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, d)
}

// (silence unused import warnings if a build tag drops something)
var _ = strings.HasPrefix
