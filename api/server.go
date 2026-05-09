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
//	GET    /v1/runs/{rid}/stream     (SSE; honours Last-Event-ID)
//	POST   /v1/runs/{rid}/cancel
//	POST   /v1/approvals/{aid}
//	GET    /v1/debug/agents
//	GET    /v1/debug/tools
package api

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
)

//go:embed web/*
var webFS embed.FS

// Server wires the runtime into an http.Handler.
type Server struct {
	Orch  *runtime.Orchestrator
	Store store.Store
}

func NewServer(orch *runtime.Orchestrator, s store.Store) *Server {
	return &Server{Orch: orch, Store: s}
}

// Handler returns an http.Handler with all v1 routes wired.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /", s.debugConsole)
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{sid}", s.getSession)
	mux.HandleFunc("GET /v1/sessions/{sid}/messages", s.listMessages)
	mux.HandleFunc("GET /v1/sessions/{sid}/runs", s.listRuns)
	mux.HandleFunc("POST /v1/sessions/{sid}/messages", s.postMessage)
	mux.HandleFunc("POST /v1/runs", s.startRun)
	mux.HandleFunc("GET /v1/runs/{rid}", s.getRun)
	mux.HandleFunc("GET /v1/runs/{rid}/events", s.listEvents)
	mux.HandleFunc("GET /v1/runs/{rid}/stream", s.streamEvents)
	mux.HandleFunc("POST /v1/runs/{rid}/cancel", s.cancelRun)
	mux.HandleFunc("POST /v1/approvals/{aid}", s.resolveApproval)
	mux.HandleFunc("GET /v1/debug/agents", s.listAgents)
	mux.HandleFunc("GET /v1/debug/tools", s.listTools)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
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
	SessionID      core.SessionID    `json:"session_id"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Workflow       runtime.Workflow  `json:"workflow"`
	Input          map[string]string `json:"input,omitempty"`
	Limits         core.Limits       `json:"limits,omitempty"`
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

// tenantOf resolves the tenant ID from headers or returns the default.
// Single-tenant deployments can ignore the header entirely.
func tenantOf(r *http.Request) core.TenantID {
	if t := r.Header.Get("X-Tenant-ID"); t != "" {
		return core.TenantID(t)
	}
	return core.DefaultTenant
}

func principalOf(r *http.Request) core.PrincipalID {
	if p := r.Header.Get("X-Principal-ID"); p != "" {
		return core.PrincipalID(p)
	}
	return core.DefaultPrincipal
}

// ---- handlers ----

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
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
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
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
		_ = s.Store.AddMessage(r.Context(), store.MessageProjection{
			SessionID:  sid,
			EventID:    out[0].ID,
			Role:       req.Role,
			Visibility: core.VisibilityParticipant,
			Content:    req.Content,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event": out[0]})
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var req startRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	if req.SessionID == "" {
		writeErr(w, http.StatusBadRequest, "validation_error", "session_id required")
		return
	}
	if len(req.Workflow.Stages) == 0 {
		writeErr(w, http.StatusBadRequest, "validation_error", "workflow.stages required")
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
		Input:          req.Input,
		Limits:         req.Limits,
	})
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
		if e.Visibility == core.VisibilityHiddenReasoning || e.Visibility == core.VisibilityAudit {
			continue
		}
		visible = append(visible, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": visible})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	rid := core.RunID(r.PathValue("rid"))
	if err := s.Orch.Cancel(r.Context(), rid, "api request"); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	aid := core.ApprovalID(r.PathValue("aid"))
	var req struct {
		Decision string          `json:"decision"`
		Reason   string          `json:"reason"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	if req.Decision != "approved" && req.Decision != "rejected" {
		writeErr(w, http.StatusBadRequest, "validation_error", "decision must be approved|rejected")
		return
	}
	if err := s.Store.ResolveApproval(r.Context(), tenantOf(r), aid, req.Decision, req.Payload); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
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

	// 1. Drain history first.
	history, _ := s.Store.ListRunEvents(r.Context(), tenant, rid, afterSeq, 0)
	for _, ev := range history {
		writeSSE(w, ev)
		afterSeq = ev.Sequence
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
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Sequence <= afterSeq {
				continue
			}
			writeSSE(w, ev)
			afterSeq = ev.Sequence
			flusher.Flush()
			if ev.Type == core.EvRunStateChanged {
				var p struct {
					Status core.RunStatus `json:"status"`
				}
				_ = json.Unmarshal(ev.Payload, &p)
				if p.Status.Terminal() {
					return
				}
			}
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev core.Event) {
	if ev.Visibility == core.VisibilityHiddenReasoning || ev.Visibility == core.VisibilityAudit {
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
	}
	body, _ := json.Marshal(envelope)
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Sequence, ev.Type, body)
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
