// Package api exposes the agent-loop runtime over HTTP REST + SSE.
//
// The handler set covers the first-slice contract from
// docs/04-contracts-and-tests.md:
//
//	GET    /v1/agents
//	PUT    /v1/agents/{name}         (admin)
//	DELETE /v1/agents/{name}         (admin)
//	POST   /v1/sessions
//	GET    /v1/sessions/{sid}
//	POST   /v1/sessions/{sid}/memories   (ingest a document into memory)
//	POST   /v1/artifacts/{aid}/memories  (ingest a text artifact into memory)
//	GET    /v1/sessions/{sid}/participants
//	PUT    /v1/sessions/{sid}/participants/{pid}
//	DELETE /v1/sessions/{sid}/participants/{pid}
//	POST   /v1/sessions/{sid}/messages
//	POST   /v1/runs
//	GET    /v1/runs/{rid}
//	GET    /v1/runs/{rid}/events?after=<seq>
//	GET    /v1/runs/{rid}/stream     (SSE; honours Last-Event-ID; ?deltas=1 adds reply deltas)
//	GET    /v1/runs/{rid}/artifacts
//	GET    /v1/artifacts/{aid}       (the content, as an attachment)
//	GET    /v1/artifacts/{aid}/versions
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
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
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
	"github.com/nvcnvn/agent-loop/policy"
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
	// Visibility is which event and message visibilities each role reads; NewServer sets policy.DefaultVisibility.
	Visibility policy.Visibility
	// Memory ingests documents and artifacts into long-term memory; nil answers those routes 501.
	Memory Ingester

	streamsClosed chan struct{}
	closeStreams  sync.Once
}

func NewServer(orch *runtime.Orchestrator, s store.Store) *Server {
	return &Server{
		Orch: orch, Store: s, Auth: HeaderIdentity, MaxBodyBytes: 1 << 20, Log: slog.Default(), Metrics: orch.Metrics,
		Visibility: policy.DefaultVisibility(), streamsClosed: make(chan struct{}),
	}
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
	mux.HandleFunc("GET /v1/agents", s.listTenantAgents)
	mux.HandleFunc("PUT /v1/agents/{name}", s.putAgent)
	mux.HandleFunc("DELETE /v1/agents/{name}", s.deleteAgent)
	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{sid}", s.getSession)
	mux.HandleFunc("GET /v1/sessions/{sid}/messages", s.listMessages)
	mux.HandleFunc("GET /v1/sessions/{sid}/runs", s.listRuns)
	mux.HandleFunc("GET /v1/sessions/{sid}/participants", s.listParticipants)
	mux.HandleFunc("POST /v1/sessions/{sid}/memories", s.ingestDocument)
	mux.HandleFunc("POST /v1/artifacts/{aid}/memories", s.ingestArtifact)
	mux.HandleFunc("PUT /v1/sessions/{sid}/participants/{pid}", s.putParticipant)
	mux.HandleFunc("DELETE /v1/sessions/{sid}/participants/{pid}", s.removeParticipant)
	mux.HandleFunc("POST /v1/sessions/{sid}/messages", s.postMessage)
	mux.HandleFunc("POST /v1/runs", s.startRun)
	mux.HandleFunc("GET /v1/runs/{rid}", s.getRun)
	mux.HandleFunc("GET /v1/runs/{rid}/events", s.listEvents)
	mux.HandleFunc(streamRoute, s.streamEvents)
	mux.HandleFunc("GET /v1/runs/{rid}/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /v1/artifacts/{aid}", s.downloadArtifact)
	mux.HandleFunc("GET /v1/artifacts/{aid}/versions", s.listArtifactVersions)
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

// maxProjectBytes caps a session's project label.
const maxProjectBytes = 200

type createSessionReq struct {
	Title   string `json:"title"`
	Project string `json:"project"`
}
type createSessionResp struct {
	SessionID core.SessionID `json:"session_id"`
}

type postMessageReq struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Run, when set, starts a run in the session with the message as input "message".
	Run *struct {
		IdempotencyKey string            `json:"idempotency_key,omitempty"`
		Workflow       runtime.Workflow  `json:"workflow"`
		Template       string            `json:"template,omitempty"`
		Input          map[string]string `json:"input,omitempty"`
		Limits         core.Limits       `json:"limits,omitempty"`
	} `json:"run,omitempty"`
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
	if !allowRole(w, identityOf(r).Role.CreatesSessions()) {
		return
	}
	var req createSessionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeDecodeErr(w, err)
		return
	}
	req.Project = strings.TrimSpace(req.Project)
	if len(req.Project) > maxProjectBytes {
		writeErr(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("project is longer than %d bytes", maxProjectBytes))
		return
	}
	id, err := s.Store.CreateSession(r.Context(), store.Session{
		TenantID:  tenantOf(r),
		CreatedBy: principalOf(r),
		Title:     req.Title,
		Project:   req.Project,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, createSessionResp{SessionID: id})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sid := core.SessionID(r.PathValue("sid"))
	if !s.allowSession(w, r, sid, needRead) {
		return
	}
	sess, err := s.Store.GetSession(r.Context(), tenantOf(r), sid)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	var participant core.PrincipalID
	if !id.Role.Operates() {
		participant = id.Principal
	}
	sessions, err := s.Store.ListSessions(r.Context(), id.Tenant, participant)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if project := r.URL.Query().Get("project"); project != "" {
		sessions = slices.DeleteFunc(sessions, func(sess store.Session) bool { return sess.Project != project })
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	sid := core.SessionID(r.PathValue("sid"))
	if !s.allowSession(w, r, sid, needRead) {
		return
	}
	messages, err := s.Store.ListMessages(r.Context(), tenantOf(r), sid)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	messages = slices.DeleteFunc(messages, func(m store.MessageProjection) bool { return !s.visible(r, m.Visibility) })
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	sid := core.SessionID(r.PathValue("sid"))
	if !s.allowSession(w, r, sid, needRead) {
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
	if !s.allowSession(w, r, sid, needWrite) {
		return
	}
	var run *runtime.StartRunInput
	if req.Run != nil {
		if len(req.Run.Workflow.Stages) == 0 && req.Run.Template == "" {
			writeErr(w, http.StatusBadRequest, "validation_error", "run needs workflow.stages or template")
			return
		}
		run = &runtime.StartRunInput{
			IdempotencyKey: cmp.Or(req.Run.IdempotencyKey, r.Header.Get("Idempotency-Key")),
			Workflow:       req.Run.Workflow, Template: req.Run.Template, Input: req.Run.Input, Limits: req.Run.Limits,
		}
	}
	posted, err := s.Orch.PostMessage(r.Context(), tenant, sid, principalOf(r), req.Role, req.Content, run)
	if err != nil && posted.Event.ID == "" {
		writeRunErr(w, err)
		return
	}
	resp := map[string]any{"event": posted.Event}
	if posted.Answered != "" {
		resp["answered_run_id"], resp["answered_approval_id"] = posted.Answered, posted.Approval
	}
	if posted.RunID != "" {
		resp["run_id"] = posted.RunID
	}
	if posted.Replayed {
		delete(resp, "event")
		resp["replayed"] = true
	}
	if err != nil {
		status, code := runErrStatus(err)
		resp["run_error"] = errorResp{Code: code, Message: err.Error(), Retryable: status == http.StatusServiceUnavailable}
	}
	writeJSON(w, http.StatusCreated, resp)
}

// runErrStatus maps an error starting a run to its HTTP status and code.
func runErrStatus(err error) (int, string) {
	switch {
	case errors.Is(err, core.ErrOverloaded):
		return http.StatusServiceUnavailable, "overloaded"
	case errors.Is(err, core.ErrConflict):
		return http.StatusConflict, "conflict"
	}
	return http.StatusBadRequest, "validation_error"
}

func writeRunErr(w http.ResponseWriter, err error) {
	status, code := runErrStatus(err)
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	writeErr(w, status, code, err.Error())
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
	if !s.allowSession(w, r, req.SessionID, needWrite) {
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
	if err != nil {
		writeRunErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, startRunResp{RunID: runID})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.allowRun(w, r, core.RunID(r.PathValue("rid")), needRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	rid := core.RunID(r.PathValue("rid"))
	if _, ok := s.allowRun(w, r, rid, needRead); !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.Store.ListRunEvents(r.Context(), tenantOf(r), rid, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	hidden := s.Visibility.HiddenPayload(identityOf(r).Role)
	visible := events[:0]
	for _, e := range events {
		if !s.visible(r, e.Visibility) {
			continue
		}
		if !hidden {
			e.HiddenPayload = nil
		}
		visible = append(visible, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": visible})
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	rid := core.RunID(r.PathValue("rid"))
	if _, ok := s.allowRun(w, r, rid, needRead); !ok {
		return
	}
	// ponytail: the store reads every artifact's content to list them; add a metadata-only query if runs hold large files.
	artifacts, err := s.Store.ListRunArtifacts(r.Context(), tenantOf(r), rid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts})
}

// downloadArtifact serves an artifact as an attachment that browsers neither
// sniff nor run, since tools and models choose its content and MIME type.
func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	if uuid.Validate(aid) != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such artifact")
		return
	}
	a, err := s.Store.GetArtifact(r.Context(), tenantOf(r), aid)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.allowSession(w, r, a.SessionID, needRead) {
		return
	}
	contentType := "application/octet-stream"
	if _, _, err := mime.ParseMediaType(a.MimeType); err == nil {
		contentType = a.MimeType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", cmp.Or(mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}), "attachment"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Content)))
	_, _ = w.Write(a.Content)
}

// listArtifactVersions lists every version of the artifact's name in its session.
func (s *Server) listArtifactVersions(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	if uuid.Validate(aid) != nil {
		writeNotFound(w)
		return
	}
	a, err := s.Store.GetArtifact(r.Context(), tenantOf(r), aid)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.allowSession(w, r, a.SessionID, needRead) {
		return
	}
	versions, err := s.Store.ListArtifactVersions(r.Context(), a.TenantID, a.SessionID, a.Name)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": versions})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	rid := core.RunID(r.PathValue("rid"))
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeDecodeErr(w, err)
		return
	}
	if len(req.Reason) > 1024 {
		writeErr(w, http.StatusBadRequest, "validation_error", "reason is longer than 1024 bytes")
		return
	}
	if _, ok := s.allowRun(w, r, rid, needWrite); !ok {
		return
	}
	switch err := s.Orch.Cancel(r.Context(), tenantOf(r), rid, principalOf(r), req.Reason); {
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
	rid := core.RunID(r.PathValue("rid"))
	if _, ok := s.allowRun(w, r, rid, needWrite); !ok {
		return
	}
	res, err := s.Orch.ReExecute(r.Context(), tenantOf(r), rid, principalOf(r))
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
	if _, ok := s.allowRun(w, r, rid, needWrite); !ok {
		return
	}
	switch err := s.Orch.Resume(r.Context(), tenantOf(r), rid, principalOf(r)); {
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
		if _, ok := s.allowRun(w, r, f.RunID, needRead); !ok {
			return
		}
	}
	if id := identityOf(r); !id.Role.Operates() {
		f.Participant = id.Principal
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
	if uuid.Validate(string(aid)) != nil {
		writeNotFound(w)
		return
	}
	approval, err := s.Store.GetApproval(r.Context(), tenantOf(r), aid)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if _, ok := s.allowRun(w, r, approval.RunID, needWrite); !ok {
		return
	}
	switch err := s.Store.ResolveApproval(r.Context(), tenantOf(r), aid, req.Decision, principalOf(r), req.Payload); {
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

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !allowRole(w, identityOf(r).Role.Operates()) {
		return
	}
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

func (s *Server) listTools(w http.ResponseWriter, r *http.Request) {
	if !allowRole(w, identityOf(r).Role.Operates()) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": s.Orch.ListTools(tenantOf(r))})
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
	if _, ok := s.allowRun(w, r, rid, needRead); !ok {
		return
	}
	visible := func(v core.Visibility) bool { return s.visible(r, v) }
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

	afterSeq, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if err != nil {
		afterSeq, _ = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	}

	// 1. Drain history first. A terminal event followed by others ended an
	// attempt of a run that was resumed later.
	history, _ := s.Store.ListRunEvents(r.Context(), tenant, rid, afterSeq, 0)
	for i, ev := range history {
		writeSSE(w, ev, visible)
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
			writeSSE(w, ev, visible)
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

func writeSSE(w http.ResponseWriter, ev core.Event, visible func(core.Visibility) bool) {
	if !visible(ev.Visibility) {
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
		"actor_id":       ev.ActorID,
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
