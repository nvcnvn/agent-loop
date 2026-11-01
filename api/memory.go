package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Ingester saves documents as long-term memory; *memory.Tools is one.
type Ingester interface {
	Ingest(ctx context.Context, tenant core.TenantID, session core.SessionID, scope store.MemoryScope, text string, src store.MemorySource) ([]string, error)
}

type ingestReq struct {
	Name    string            `json:"name"`
	Content string            `json:"content"`
	Scope   store.MemoryScope `json:"scope"`
}

// ingestDocument saves a document posted with the request as session memory.
func (s *Server) ingestDocument(w http.ResponseWriter, r *http.Request) {
	sid := core.SessionID(r.PathValue("sid"))
	var req ingestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if !s.allowSession(w, r, sid, needWrite) {
		return
	}
	s.ingest(w, r, sid, req.Scope, req.Content, store.MemorySource{Document: req.Name})
}

// ingestArtifact saves a text artifact's content as memory of its session.
func (s *Server) ingestArtifact(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	var req ingestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeDecodeErr(w, err)
		return
	}
	if uuid.Validate(aid) != nil {
		writeNotFound(w)
		return
	}
	a, err := s.Store.GetArtifact(r.Context(), tenantOf(r), aid)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.allowSession(w, r, a.SessionID, needWrite) {
		return
	}
	if !textual(a.MimeType) || !utf8.Valid(a.Content) {
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "only text and JSON artifacts can be ingested")
		return
	}
	s.ingest(w, r, a.SessionID, req.Scope, string(a.Content), store.MemorySource{ArtifactID: a.ID, Document: a.Name, RunID: a.RunID, TaskID: a.TaskID, StepID: a.StepID})
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request, sid core.SessionID, scope store.MemoryScope, text string, src store.MemorySource) {
	if s.Memory == nil {
		writeErr(w, http.StatusNotImplemented, "not_implemented", "long-term memory is not configured")
		return
	}
	if scope == "" {
		scope = store.MemorySession
	}
	if scope == store.MemoryTenant && !allowRole(w, identityOf(r).Role.Operates()) {
		return
	}
	src.By = principalOf(r)
	ids, err := s.Memory.Ingest(r.Context(), tenantOf(r), sid, scope, text, src)
	if err != nil {
		status, code := http.StatusInternalServerError, "internal_error"
		switch {
		case errors.Is(err, store.ErrInvalidMemory):
			status, code = http.StatusBadRequest, "validation_error"
		case errors.Is(err, store.ErrMemoryTooLarge):
			status, code = http.StatusRequestEntityTooLarge, "validation_error"
		}
		writeErr(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"memories": ids, "scope": scope})
}

func textual(mimeType string) bool {
	t, _, err := mime.ParseMediaType(mimeType)
	return err == nil && (strings.HasPrefix(t, "text/") || t == "application/json" || strings.HasSuffix(t, "+json"))
}
