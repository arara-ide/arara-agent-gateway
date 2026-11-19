package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arara-ide/arara-agent-gateway/internal/agent"
	"github.com/arara-ide/arara-agent-gateway/internal/github"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/run"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

type instanceView struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"created_at"`
	LastActivityAt time.Time  `json:"last_activity_at"`
	IdleStopAt     *time.Time `json:"idle_stop_at,omitempty"`
	SelectedRepo   string     `json:"selected_repo,omitempty"`
	Agents         []string   `json:"agents"`
	ActiveRuns     int        `json:"active_runs"`
	RunningAgents  []string   `json:"running_agents"`
	LastError      string     `json:"last_error,omitempty"`
}

func (s *Server) view(in store.Instance) instanceView {
	v := instanceView{ID: in.ID, Name: in.Name, State: string(in.State), CreatedAt: in.CreatedAt, LastActivityAt: in.LastActivityAt, SelectedRepo: in.SelectedRepo, Agents: agent.Names(), RunningAgents: s.Runs.ActiveAgents(in.ID), LastError: in.LastError}
	v.ActiveRuns = len(v.RunningAgents)
	if in.State == store.StateRunning && s.Instances.Busy(in.ID) == 0 {
		t := in.LastActivityAt.Add(s.IdleTimeout)
		v.IdleStopAt = &t
	}
	return v
}

type connectionView struct {
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	Method    string    `json:"method,omitempty"`
	Account   string    `json:"account,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

func connView(c store.Connection) connectionView {
	return connectionView{Provider: c.Provider, Status: string(c.Status), Method: c.Method, Account: c.Account, Error: c.Error, UpdatedAt: c.UpdatedAt}
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) error {
	out := []map[string]any{}
	for _, name := range agent.Names() {
		a, _ := agent.Get(name)
		out = append(out, map[string]any{"name": name, "title": a.Title(), "methods": a.Methods(), "login": a.Login() != nil})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
	return nil
}

func (s *Server) listInstances(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListInstances(r.Context())
	if err != nil {
		return err
	}
	out := make([]instanceView, 0, len(list))
	for _, in := range list {
		out = append(out, s.view(in))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": out})
	return nil
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeOptional(r, &body); err != nil {
		return err
	}
	name, err := cleanName(body.Name)
	if err != nil {
		return err
	}
	in, err := s.Instances.Create(r.Context(), name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{"instance": s.view(in)})
	return nil
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 80 || strings.ContainsAny(name, "\x00\r\n") {
		return "", newError(http.StatusBadRequest, "invalid_name", "Name must have at most 80 characters on one line")
	}
	return name, nil
}

func (s *Server) getInstance(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": s.view(in)})
	return nil
}

func (s *Server) updateInstance(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	var body struct {
		Name         *string `json:"name"`
		SelectedRepo *string `json:"selected_repo"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if body.Name != nil {
		n, err := cleanName(*body.Name)
		if err != nil {
			return err
		}
		body.Name = &n
	}
	if body.SelectedRepo != nil && *body.SelectedRepo != "" {
		if _, _, err := github.ParseFullName(*body.SelectedRepo); err != nil {
			return err
		}
	}
	if err := s.Store.UpdateInstance(r.Context(), in.ID, body.Name, body.SelectedRepo); err != nil {
		return err
	}
	in, err = s.Store.GetInstance(r.Context(), in.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": s.view(in)})
	return nil
}

func (s *Server) deleteInstance(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	if r.URL.Query().Get("confirm") != in.ID {
		return errConfirm
	}
	if err := s.Instances.Delete(r.Context(), in.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) startInstance(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	in, err = s.Instances.Start(context.WithoutCancel(r.Context()), in.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": s.view(in)})
	return nil
}

func (s *Server) stopInstance(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	in, err = s.Instances.Stop(context.WithoutCancel(r.Context()), in.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": s.view(in)})
	return nil
}

func (s *Server) agentConnections(ctx context.Context, in store.Instance) []connectionView {
	out := []connectionView{}
	for _, name := range agent.Names() {
		a, _ := agent.Get(name)
		out = append(out, connView(s.Connections.Probe(ctx, in, a)))
	}
	return out
}

func (s *Server) instanceStatus(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	gh, err := s.GitHub.Status(r.Context(), in.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instance": s.view(in),
		"agents":   s.agentConnections(r.Context(), in),
		"github":   connView(gh),
	})
	return nil
}

func (s *Server) instanceAgents(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": s.agentConnections(r.Context(), in)})
	return nil
}

func (s *Server) instanceConnections(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	out := []connectionView{}
	for _, p := range append([]string{github.Provider}, agent.Names()...) {
		c := s.Connections.Connection(r.Context(), in.ID, p)
		if p == github.Provider && c.Status == store.ConnSetupRequired {
			c.Status = store.ConnDisconnected
		}
		out = append(out, connView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance_id": in.ID, "connections": out})
	return nil
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	runs, err := s.Store.ListRuns(r.Context(), in.ID)
	if err != nil {
		return err
	}
	out := make([]run.View, 0, len(runs))
	for _, rr := range runs {
		out = append(out, run.RunView(rr))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out, "limit": store.HistoryLimit})
	return nil
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	var body struct {
		Prompt    string `json:"prompt"`
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
		Repo      string `json:"repo"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if _, ok := agent.Get(r.PathValue("agent")); !ok {
		return run.ErrUnknownAgent
	}
	if err := run.Validate(run.Request{Prompt: body.Prompt, SessionID: body.SessionID, Model: body.Model}); err != nil {
		return err
	}
	if body.Repo != "" {
		if _, _, err := github.ParseFullName(body.Repo); err != nil {
			return err
		}
	}
	ctx := context.WithoutCancel(r.Context())
	if in.State != store.StateRunning && s.AutoStart {
		if in, err = s.Instances.Start(ctx, in.ID); err != nil {
			return err
		}
	}
	rr, err := s.Runs.Start(ctx, in, r.PathValue("agent"), run.Request{Prompt: body.Prompt, SessionID: body.SessionID, Model: body.Model, Repo: body.Repo})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/instances/"+in.ID+"/runs/"+rr.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run.RunView(rr), "stream_url": "/api/instances/" + in.ID + "/runs/" + rr.ID + "/stream"})
	return nil
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	rr, err := s.Store.GetRun(r.Context(), in.ID, r.PathValue("runId"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run.RunView(rr)})
	return nil
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	rr, err := s.Store.GetRun(r.Context(), in.ID, r.PathValue("runId"))
	if err != nil {
		return err
	}
	if err := s.Runs.Cancel(in.ID, rr.ID, "cancelled_by_request"); err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run.RunView(rr), "cancelling": true})
	return nil
}

func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	l, err := s.Connections.StartLogin(r.Context(), in, r.PathValue("agent"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"login": l})
	return nil
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	l, err := s.Connections.GetLogin(in.ID, r.PathValue("agent"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": l})
	return nil
}

func (s *Server) loginInput(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	l, err := s.Connections.SendLoginInput(in.ID, r.PathValue("agent"), body.Code)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"login": l})
	return nil
}

func (s *Server) cancelLogin(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	l, err := s.Connections.CancelLogin(in.ID, r.PathValue("agent"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": l})
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	c, err := s.Connections.Logout(context.WithoutCancel(r.Context()), in, r.PathValue("agent"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": connView(c)})
	return nil
}

func (s *Server) listLocalRepos(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	if in.State != store.StateRunning {
		return instance.ErrNotRunning
	}
	repos, err := s.Workspace.List(r.Context(), in)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos, "selected_repo": in.SelectedRepo})
	return nil
}

func (s *Server) withInstanceWork(r *http.Request, in store.Instance, fn func(ctx context.Context) error) error {
	ctx := context.WithoutCancel(r.Context())
	release, err := s.Instances.Acquire(ctx, in.ID)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

func (s *Server) syncRepo(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	var body struct {
		FullName string `json:"full_name"`
		Select   bool   `json:"select"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if _, _, err := github.ParseFullName(body.FullName); err != nil {
		return err
	}
	var repo github.LocalRepo
	err = s.withInstanceWork(r, in, func(ctx context.Context) error {
		var err error
		repo, err = s.Workspace.Sync(ctx, in, body.FullName)
		if err != nil || !body.Select {
			return err
		}
		return s.Store.UpdateInstance(ctx, in.ID, nil, &repo.FullName)
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "selected": body.Select})
	return nil
}

func (s *Server) pushRepo(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	var body struct {
		Agent  string `json:"agent"`
		Branch string `json:"branch"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if _, ok := agent.Get(body.Agent); !ok {
		return run.ErrUnknownAgent
	}
	full := r.PathValue("owner") + "/" + r.PathValue("name")
	if _, _, err := github.ParseFullName(full); err != nil {
		return err
	}
	var branch string
	err = s.withInstanceWork(r, in, func(ctx context.Context) error {
		var err error
		branch, err = s.Workspace.Push(ctx, in, full, body.Agent, body.Branch)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": full, "branch": branch, "pushed": true})
	return nil
}

func (s *Server) setAPIKey(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	c, err := s.Connections.SetAPIKey(r.Context(), in.ID, r.PathValue("agent"), body.APIKey)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": connView(c)})
	return nil
}

func (s *Server) removeAPIKey(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	c, err := s.Connections.RemoveAPIKey(r.Context(), in.ID, r.PathValue("agent"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": connView(c)})
	return nil
}

func (s *Server) githubConnect(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	u, exp, err := s.GitHub.Connect(r.Context(), in.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorize_url": u, "expires_at": exp})
	return nil
}

func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	var body struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	c, err := s.GitHub.Complete(r.Context(), in.ID, body.Code, body.State)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": connView(c)})
	return nil
}

func (s *Server) githubStatus(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	c, err := s.GitHub.Status(r.Context(), in.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": connView(c), "enabled": s.GitHub.Enabled()})
	return nil
}

func (s *Server) githubRepos(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 || page > 100 {
		page = 1
	}
	repos, more, err := s.GitHub.Repos(r.Context(), in.ID, page)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos, "page": page, "has_more": more})
	return nil
}

func (s *Server) githubDisconnect(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	c, err := s.GitHub.Disconnect(r.Context(), in.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": connView(c)})
	return nil
}
