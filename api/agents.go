package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/runtime"
)

// listTenantAgents lists the agents the caller's runs may use; only admins
// and operators see their instructions.
func (s *Server) listTenantAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.Orch.ListAgents(r.Context(), tenantOf(r))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	for i := range agents {
		if !identityOf(r).Role.Operates() || agents[i].Source == "config" && !s.Debug {
			agents[i].Instructions = ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) putAgent(w http.ResponseWriter, r *http.Request) {
	if !allowRole(w, identityOf(r).Role.ManagesAgents()) {
		return
	}
	var a runtime.Agent
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		writeDecodeErr(w, err)
		return
	}
	a.Name = r.PathValue("name")
	version, err := s.Orch.PutAgent(r.Context(), tenantOf(r), a)
	switch {
	case errors.Is(err, runtime.ErrInvalidAgent):
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
	case errors.Is(err, core.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case err != nil:
		writeStoreErr(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"name": a.Name, "version": version})
	}
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if !allowRole(w, identityOf(r).Role.ManagesAgents()) {
		return
	}
	switch err := s.Orch.DeleteAgent(r.Context(), tenantOf(r), r.PathValue("name")); {
	case errors.Is(err, core.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case err != nil:
		writeStoreErr(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
