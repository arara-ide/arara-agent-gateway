package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/arara-ide/arara-agent-gateway/internal/connect"
	"github.com/arara-ide/arara-agent-gateway/internal/github"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/run"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func newError(status int, code, message string) *apiError {
	return &apiError{status: status, code: code, message: message}
}

var (
	errUnauthorized = newError(http.StatusUnauthorized, "unauthorized", "Missing or invalid apikey header")
	errNotFound     = newError(http.StatusNotFound, "not_found", "Resource not found")
	errBadJSON      = newError(http.StatusBadRequest, "invalid_json", "Request body must be a valid JSON object with known fields")
	errConfirm      = newError(http.StatusBadRequest, "confirmation_required", "Pass confirm=<instance id> to delete an instance")
)

var errorTable = []struct {
	target error
	err    *apiError
}{
	{store.ErrNotFound, errNotFound},
	{instance.ErrNotRunning, newError(http.StatusConflict, "instance_not_running", "Start the instance before this operation")},
	{instance.ErrCapacity, newError(http.StatusTooManyRequests, "capacity_reached", "Maximum number of active instances reached")},
	{instance.ErrImageMissing, newError(http.StatusServiceUnavailable, "runtime_image_missing", "The runtime image is not available on the Docker host")},
	{run.ErrUnknownAgent, newError(http.StatusNotFound, "unknown_agent", "Unknown agent")},
	{connect.ErrUnknownAgent, newError(http.StatusNotFound, "unknown_agent", "Unknown agent")},
	{run.ErrInvalidPrompt, newError(http.StatusBadRequest, "invalid_prompt", run.ErrInvalidPrompt.Error())},
	{run.ErrInvalidSession, newError(http.StatusBadRequest, "invalid_session_id", "Invalid session_id")},
	{run.ErrInvalidModel, newError(http.StatusBadRequest, "invalid_model", "Invalid model")},
	{run.ErrAgentBusy, newError(http.StatusConflict, "agent_busy", run.ErrAgentBusy.Error())},
	{run.ErrInstanceBusy, newError(http.StatusTooManyRequests, "instance_busy", run.ErrInstanceBusy.Error())},
	{run.ErrNotAuthenticated, newError(http.StatusConflict, "agent_not_authenticated", "Connect this agent in this instance before running it")},
	{run.ErrRunFinished, newError(http.StatusConflict, "run_not_active", "The run is not active")},
	{connect.ErrMethodUnsupported, newError(http.StatusBadRequest, "method_not_supported", "This connection method is not supported by the agent")},
	{connect.ErrInvalidAPIKey, newError(http.StatusBadRequest, "invalid_api_key", "The API key format is invalid")},
	{connect.ErrInvalidInput, newError(http.StatusBadRequest, "invalid_login_input", "The login code is invalid")},
	{connect.ErrLoginNotFound, newError(http.StatusNotFound, "login_not_found", "No login session for this agent")},
	{connect.ErrLoginNoInput, newError(http.StatusConflict, "login_input_not_expected", "This login session is not waiting for a code")},
	{github.ErrDisabled, newError(http.StatusServiceUnavailable, "github_not_configured", "GitHub integration is not configured")},
	{github.ErrInvalidState, newError(http.StatusBadRequest, "github_invalid_state", "Invalid or expired GitHub authorization")},
	{github.ErrNotConnected, newError(http.StatusConflict, "github_not_connected", "Connect a GitHub account first")},
	{github.ErrExpired, newError(http.StatusConflict, "github_expired", "GitHub credentials expired or were revoked")},
	{github.ErrInvalidRepo, newError(http.StatusBadRequest, "invalid_repo", "Repository must be owner/name")},
	{github.ErrRepoForbidden, newError(http.StatusForbidden, "repo_forbidden", "Repository is not accessible with this GitHub connection")},
	{github.ErrRepoNotCloned, newError(http.StatusConflict, "repo_not_cloned", "Clone the repository in the instance first")},
	{github.ErrInvalidBranch, newError(http.StatusBadRequest, "invalid_branch", github.ErrInvalidBranch.Error())},
	{github.ErrPushNotAllowed, newError(http.StatusForbidden, "push_not_allowed", github.ErrPushNotAllowed.Error())},
	{github.ErrGitFailed, newError(http.StatusBadGateway, "git_failed", "Git command failed inside the instance")},
	{github.ErrUpstream, newError(http.StatusBadGateway, "github_unavailable", "GitHub request failed")},
}

func toAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	for _, e := range errorTable {
		if errors.Is(err, e.target) {
			return e.err
		}
	}
	return newError(http.StatusInternalServerError, "internal_error", "Internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err *apiError) {
	writeJSON(w, err.status, map[string]any{"error": map[string]string{"code": err.code, "message": err.message}})
}
