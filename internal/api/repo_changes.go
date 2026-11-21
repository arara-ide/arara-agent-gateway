package api

import (
	"net/http"

	"github.com/arara-ide/arara-agent-gateway/internal/agent"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/run"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

func (s *Server) repoChanges(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-store")
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	if in.State != store.StateRunning {
		return instance.ErrNotRunning
	}
	query := r.URL.Query()
	agentName := query.Get("agent")
	if _, ok := agent.Get(agentName); !ok {
		return run.ErrUnknownAgent
	}
	includeDiff := query.Get("include_diff")
	if includeDiff != "" && includeDiff != "false" && includeDiff != "true" {
		return newError(http.StatusBadRequest, "invalid_include_diff", "include_diff must be true or false")
	}
	changes, err := s.Workspace.Changes(r.Context(), in, query.Get("full_name"), agentName, includeDiff == "true")
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, changes)
	return nil
}
