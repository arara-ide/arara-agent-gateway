package run

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/arara-ide/arara-agent-gateway/internal/agent"
	"github.com/arara-ide/arara-agent-gateway/internal/connect"
	"github.com/arara-ide/arara-agent-gateway/internal/docker"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const (
	MaxPromptBytes   = 100 << 10
	maxResponseBytes = 256 << 10
	maxLineBytes     = 8 << 20
	hubRetention     = 2 * time.Minute
	stopWait         = 15 * time.Second
)

var (
	ErrUnknownAgent     = errors.New("unknown agent")
	ErrInvalidPrompt    = errors.New("prompt must be between 1 and 102400 bytes of valid UTF-8")
	ErrInvalidSession   = errors.New("invalid session_id")
	ErrInvalidModel     = errors.New("invalid model")
	ErrAgentBusy        = errors.New("this agent already has an active run in the instance")
	ErrInstanceBusy     = errors.New("maximum number of active agents for this instance reached")
	ErrNotAuthenticated = errors.New("agent is not authenticated for this user")
	ErrRunNotFound      = errors.New("run not found")
	ErrRunFinished      = errors.New("run already finished")
)

type Workspace interface {
	Prepare(ctx context.Context, in store.Instance, agentName, repo string) (string, error)
}

type Settings struct {
	MaxPerInstance int
	Timeout        time.Duration
}

type Request struct {
	Prompt    string
	SessionID string
	Model     string
	Repo      string
}

type activeRun struct {
	run    store.Run
	hub    *Hub
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	reason string
}

type Manager struct {
	store    *store.Store
	inst     *instance.Manager
	engine   docker.Engine
	conn     *connect.Service
	ws       Workspace
	settings Settings
	now      func() time.Time
	log      *slog.Logger
	mu       sync.Mutex
	active   map[string]*activeRun
	hubs     map[string]*Hub
}

func NewManager(s *store.Store, inst *instance.Manager, e docker.Engine, conn *connect.Service, ws Workspace, settings Settings, now func() time.Time, log *slog.Logger) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{store: s, inst: inst, engine: e, conn: conn, ws: ws, settings: settings, now: now, log: log, active: map[string]*activeRun{}, hubs: map[string]*Hub{}}
}

func (m *Manager) ActiveCount(instanceID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.active {
		if a.run.InstanceID == instanceID {
			n++
		}
	}
	return n
}

func (m *Manager) ActiveAgents(instanceID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for _, a := range m.active {
		if a.run.InstanceID == instanceID {
			out = append(out, a.run.Agent)
		}
	}
	return out
}

func Validate(req Request) error {
	if req.Prompt == "" || len(req.Prompt) > MaxPromptBytes || !utf8.ValidString(req.Prompt) || strings.ContainsRune(req.Prompt, 0) {
		return ErrInvalidPrompt
	}
	if req.SessionID != "" && !agent.ValidSessionID(req.SessionID) {
		return ErrInvalidSession
	}
	if req.Model != "" && !agent.ValidModel(req.Model) {
		return ErrInvalidModel
	}
	return nil
}

func (m *Manager) reserve(in store.Instance, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserveLocked(in, name)
}

func (m *Manager) Start(ctx context.Context, in store.Instance, name string, req Request) (store.Run, error) {
	a, ok := agent.Get(name)
	if !ok {
		return store.Run{}, ErrUnknownAgent
	}
	if err := Validate(req); err != nil {
		return store.Run{}, err
	}
	if req.Repo == "" {
		req.Repo = in.SelectedRepo
	}
	if err := m.reserve(in, name); err != nil {
		return store.Run{}, err
	}
	release, err := m.inst.Acquire(ctx, in.ID)
	if err != nil {
		return store.Run{}, err
	}
	creds, err := m.conn.Credentials(ctx, in.UserID, a)
	if err != nil {
		release()
		return store.Run{}, err
	}
	if !creds.UseAPIKey {
		if !supportsAccount(a) || m.conn.Probe(ctx, in, a).Status != store.ConnConnected {
			release()
			return store.Run{}, ErrNotAuthenticated
		}
	}
	cwd := instance.WorkDir + "/" + name
	if req.Repo != "" && m.ws != nil {
		cwd, err = m.ws.Prepare(ctx, in, name, req.Repo)
		if err != nil {
			release()
			return store.Run{}, err
		}
	}

	r := store.Run{ID: instance.NewID("run_"), InstanceID: in.ID, Agent: name, Prompt: req.Prompt, Status: store.RunRunning, SessionID: req.SessionID, Repo: req.Repo, Model: req.Model, StartedAt: m.now().UTC()}
	rctx, cancel := context.WithTimeout(context.Background(), m.settings.Timeout)
	act := &activeRun{run: r, hub: NewHub(), cancel: cancel, done: make(chan struct{})}

	m.mu.Lock()
	if err := m.reserveLocked(in, name); err != nil {
		m.mu.Unlock()
		cancel()
		release()
		return store.Run{}, err
	}
	m.active[r.ID] = act
	m.hubs[r.ID] = act.hub
	m.mu.Unlock()

	if err := m.store.InsertRun(ctx, r); err != nil {
		m.mu.Lock()
		delete(m.active, r.ID)
		delete(m.hubs, r.ID)
		m.mu.Unlock()
		cancel()
		release()
		return store.Run{}, err
	}
	act.hub.Publish("status", map[string]any{"run_id": r.ID, "agent": name, "status": r.Status})
	cmd := a.Run(agent.RunInput{Prompt: req.Prompt, SessionID: req.SessionID, Model: req.Model, UseAPIKey: creds.UseAPIKey})
	go m.execute(rctx, in, a, act, cmd, cwd, creds, release)
	return r, nil
}

func (m *Manager) reserveLocked(in store.Instance, name string) error {
	n := 0
	for _, a := range m.active {
		if a.run.InstanceID != in.ID {
			continue
		}
		if a.run.Agent == name {
			return ErrAgentBusy
		}
		n++
	}
	if n >= m.settings.MaxPerInstance {
		return ErrInstanceBusy
	}
	return nil
}

func supportsAccount(a agent.Adapter) bool {
	for _, m := range a.Methods() {
		if m == agent.MethodAccount {
			return true
		}
	}
	return false
}

func (m *Manager) execute(ctx context.Context, in store.Instance, a agent.Adapter, act *activeRun, cmd agent.Command, cwd string, creds connect.Credentials, release func()) {
	defer release()
	defer close(act.done)
	defer act.cancel()

	streamDone := make(chan struct{})
	go func() {
		select {
		case <-streamDone:
			return
		case <-ctx.Done():
		}
		kctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := instance.Kill(kctx, m.engine, in.ID, act.run.ID); err != nil {
			m.log.Debug("kill run process", "run", act.run.ID, "error", err)
		}
	}()

	parser := a.NewParser()
	env := append([]string{"TERM=dumb", "NO_COLOR=1", "CI=1"}, creds.Env...)
	exitCode, stderr, execErr := m.stream(ctx, in, act, cmd, cwd, env, parser)
	if ctx.Err() == nil {
		close(streamDone)
	}
	out := parser.Outcome()

	r := act.run
	r.FinishedAt = m.now().UTC()
	r.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	r.Response = clip(out.Response, maxResponseBytes)
	if out.SessionID != "" {
		r.SessionID = out.SessionID
	}
	act.mu.Lock()
	reason := act.reason
	act.mu.Unlock()

	switch {
	case reason != "":
		r.Status, r.ErrorCode, r.ErrorMessage = store.RunCancelled, reason, "The run was cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		r.Status, r.ErrorCode, r.ErrorMessage = store.RunTimedOut, "run_timeout", "The run exceeded the configured timeout"
	case out.AuthError || ((out.Failed || exitCode != 0) && a.IsAuthFailure(stderr+"\n"+out.Message)):
		r.Status, r.ErrorCode, r.ErrorMessage = store.RunFailed, "agent_not_authenticated", firstNonEmpty(out.Message, "The agent is not authenticated")
		m.conn.MarkAuthFailure(context.Background(), in.UserID, a.Name(), creds.Method)
	case execErr != nil:
		r.Status, r.ErrorCode, r.ErrorMessage = store.RunFailed, "exec_failed", "Could not execute the agent in the instance"
		m.log.Warn("run exec failed", "run", r.ID, "error", execErr)
	case out.Failed:
		r.Status, r.ErrorCode, r.ErrorMessage = store.RunFailed, "agent_error", firstNonEmpty(out.Message, "The agent reported an error")
	case exitCode != 0:
		r.Status, r.ErrorCode, r.ErrorMessage = store.RunFailed, "agent_exit", firstNonEmpty(clip(strings.TrimSpace(agent.StripANSI(stderr)), 500), "The agent exited with an error")
	case !out.Completed:
		r.Status, r.ErrorCode, r.ErrorMessage = store.RunFailed, "agent_incomplete", "The agent finished without a result"
	default:
		r.Status = store.RunSucceeded
	}

	if err := m.store.FinishRun(context.Background(), r); err != nil {
		m.log.Error("finish run", "run", r.ID, "error", err)
	}
	act.hub.Publish("done", RunView(r))
	act.hub.Close()

	m.mu.Lock()
	delete(m.active, r.ID)
	m.mu.Unlock()
	time.AfterFunc(hubRetention, func() {
		m.mu.Lock()
		delete(m.hubs, r.ID)
		m.mu.Unlock()
	})
}

func (m *Manager) stream(ctx context.Context, in store.Instance, act *activeRun, cmd agent.Command, cwd string, env []string, parser agent.Parser) (int, string, error) {
	proc, err := m.engine.Exec(ctx, docker.ExecSpec{
		Container: instance.ContainerName(in.ID),
		Cmd:       instance.Wrap(act.run.ID, cwd, cmd.Args),
		Env:       env,
		Stdin:     true,
	})
	if err != nil {
		return -1, "", err
	}
	defer proc.Close()
	if len(cmd.Stdin) > 0 {
		if _, err := proc.Write(cmd.Stdin); err != nil {
			return -1, "", err
		}
	}
	_ = proc.CloseStdin()

	reader := bufio.NewReaderSize(proc.Stdout(), 64<<10)
	for {
		line, err := readLine(reader)
		if len(line) > 0 {
			for _, ev := range parser.Line(line) {
				act.hub.Publish(ev.Type, ev)
				if ev.Type == "session" {
					m.mu.Lock()
					act.run.SessionID = ev.SessionID
					m.mu.Unlock()
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				m.log.Debug("run stream ended", "run", act.run.ID, "error", err)
			}
			break
		}
	}
	code, err := proc.Wait(ctx)
	if ctx.Err() != nil {
		err = nil
	}
	return code, proc.StderrTail(), err
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if len(buf)+len(chunk) <= maxLineBytes {
			buf = append(buf, chunk...)
		}
		if err != nil || !isPrefix {
			return buf, err
		}
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (m *Manager) Cancel(instanceID, runID, reason string) error {
	m.mu.Lock()
	act, ok := m.active[runID]
	m.mu.Unlock()
	if !ok || act.run.InstanceID != instanceID {
		return ErrRunFinished
	}
	act.mu.Lock()
	if act.reason == "" {
		act.reason = reason
	}
	act.mu.Unlock()
	act.cancel()
	return nil
}

func (m *Manager) CancelInstance(instanceID, reason string) {
	m.mu.Lock()
	var list []*activeRun
	for _, a := range m.active {
		if a.run.InstanceID == instanceID {
			list = append(list, a)
		}
	}
	m.mu.Unlock()
	for _, a := range list {
		_ = m.Cancel(instanceID, a.run.ID, reason)
	}
	deadline := time.After(stopWait)
	for _, a := range list {
		select {
		case <-a.done:
		case <-deadline:
			return
		}
	}
}

func (m *Manager) Hub(instanceID, runID string) *Hub {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.active[runID]; ok && a.run.InstanceID != instanceID {
		return nil
	}
	h := m.hubs[runID]
	return h
}

type View struct {
	ID           string     `json:"id"`
	InstanceID   string     `json:"instance_id"`
	Agent        string     `json:"agent"`
	Prompt       string     `json:"prompt"`
	Response     string     `json:"response"`
	Status       string     `json:"status"`
	SessionID    string     `json:"session_id,omitempty"`
	Repo         string     `json:"repo,omitempty"`
	Model        string     `json:"model,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	DurationMS   int64      `json:"duration_ms"`
}

func RunView(r store.Run) View {
	v := View{ID: r.ID, InstanceID: r.InstanceID, Agent: r.Agent, Prompt: r.Prompt, Response: r.Response, Status: string(r.Status), SessionID: r.SessionID, Repo: r.Repo, Model: r.Model, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage, StartedAt: r.StartedAt, DurationMS: r.DurationMS}
	if !r.FinishedAt.IsZero() {
		t := r.FinishedAt
		v.FinishedAt = &t
	}
	return v
}
