package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/store"
)

type need int

const (
	needRead need = iota
	needWrite
	needManage
)

// allowSession reports whether the caller may act on session sid as n needs,
// answering 404 when the caller may not even read it, so its existence does
// not leak, and 403 when it may read but not do more.
func (s *Server) allowSession(w http.ResponseWriter, r *http.Request, sid core.SessionID, n need) bool {
	id := identityOf(r)
	if uuid.Validate(string(sid)) != nil {
		writeNotFound(w)
		return false
	}
	if _, err := s.Store.GetSession(r.Context(), id.Tenant, sid); err != nil {
		writeStoreErr(w, err)
		return false
	}
	var role policy.SessionRole
	if !id.Role.Operates() {
		p, err := s.Store.GetParticipant(r.Context(), id.Tenant, sid, id.Principal)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			writeStoreErr(w, err)
			return false
		}
		role = policy.SessionRole(p.Role)
	}
	access := policy.Access(id.Role, role)
	switch {
	case !access.Read:
		writeNotFound(w)
	case n == needWrite && !access.Write, n == needManage && !access.Manage:
		writeErr(w, http.StatusForbidden, "forbidden", "your role in this session does not allow this")
	default:
		return true
	}
	return false
}

// allowRun is allowSession for the session of run rid, which it returns.
func (s *Server) allowRun(w http.ResponseWriter, r *http.Request, rid core.RunID, n need) (store.Run, bool) {
	if uuid.Validate(string(rid)) != nil {
		writeNotFound(w)
		return store.Run{}, false
	}
	run, err := s.Store.GetRun(r.Context(), tenantOf(r), rid)
	if err != nil {
		writeStoreErr(w, err)
		return store.Run{}, false
	}
	return run, s.allowSession(w, r, run.SessionID, n)
}

// allowRole answers 403 unless the caller's role allows the request.
func allowRole(w http.ResponseWriter, allowed bool) bool {
	if !allowed {
		writeErr(w, http.StatusForbidden, "forbidden", "your role does not allow this")
	}
	return allowed
}

// writeNotFound answers alike for what does not exist and what the caller may not see.
func writeNotFound(w http.ResponseWriter) {
	writeErr(w, http.StatusNotFound, "not_found", core.ErrNotFound.Error())
}

func writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, core.ErrNotFound) {
		writeNotFound(w)
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
}

// visible reports whether the caller's role may read events and messages of visibility v.
func (s *Server) visible(r *http.Request, v core.Visibility) bool {
	return s.Visibility.Allows(identityOf(r).Role, v)
}

func (s *Server) listParticipants(w http.ResponseWriter, r *http.Request) {
	sid := core.SessionID(r.PathValue("sid"))
	if !s.allowSession(w, r, sid, needRead) {
		return
	}
	list, err := s.Store.ListParticipants(r.Context(), tenantOf(r), sid)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"participants": list})
}

func (s *Server) putParticipant(w http.ResponseWriter, r *http.Request) {
	sid, pid := core.SessionID(r.PathValue("sid")), core.PrincipalID(r.PathValue("pid"))
	var req struct {
		Role policy.SessionRole `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if !req.Role.Valid() || uuid.Validate(string(pid)) != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "role must be owner, member or viewer, and the principal a UUID")
		return
	}
	if !s.allowSession(w, r, sid, needManage) {
		return
	}
	p := store.Participant{TenantID: tenantOf(r), SessionID: sid, PrincipalID: pid, Role: string(req.Role)}
	if err := s.Store.PutParticipant(r.Context(), p); err != nil {
		writeStoreErr(w, err)
		return
	}
	got, err := s.Store.GetParticipant(r.Context(), p.TenantID, sid, pid)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (s *Server) removeParticipant(w http.ResponseWriter, r *http.Request) {
	sid, pid := core.SessionID(r.PathValue("sid")), core.PrincipalID(r.PathValue("pid"))
	if !s.allowSession(w, r, sid, needManage) {
		return
	}
	if uuid.Validate(string(pid)) != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such participant")
		return
	}
	if err := s.Store.RemoveParticipant(r.Context(), tenantOf(r), sid, pid); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
