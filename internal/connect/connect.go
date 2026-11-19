package connect

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/agent"
	"github.com/arara-ide/arara-agent-gateway/internal/docker"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/secret"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const (
	loginTimeout   = 10 * time.Minute
	statusTimeout  = 30 * time.Second
	maxLoginOutput = 64 << 10
	maxAPIKeyLen   = 512
)

var (
	ErrUnknownAgent      = errors.New("unknown agent")
	ErrMethodUnsupported = errors.New("method not supported by this agent")
	ErrInvalidAPIKey     = errors.New("invalid api key")
	ErrInvalidInput      = errors.New("invalid login input")
	ErrLoginNotFound     = errors.New("login session not found")
	ErrLoginNoInput      = errors.New("this login does not accept input")
)

type Service struct {
	store  *store.Store
	box    *secret.Box
	inst   *instance.Manager
	engine docker.Engine
	now    func() time.Time
	log    *slog.Logger
	mu     sync.Mutex
	logins map[string]*loginSession
}

func New(s *store.Store, box *secret.Box, inst *instance.Manager, engine docker.Engine, now func() time.Time, log *slog.Logger) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: s, box: box, inst: inst, engine: engine, now: now, log: log, logins: map[string]*loginSession{}}
}

func secretAD(userID, provider string) string { return "connection:" + provider + ":" + userID }

func (s *Service) Box() *secret.Box { return s.box }

func (s *Service) Connection(ctx context.Context, userID, provider string) store.Connection {
	c, err := s.store.GetConnection(ctx, userID, provider)
	if err != nil {
		return store.Connection{UserID: userID, Provider: provider, Status: store.ConnSetupRequired}
	}
	return c
}

func (s *Service) SetAPIKey(ctx context.Context, userID, name, key string) (store.Connection, error) {
	a, ok := agent.Get(name)
	if !ok {
		return store.Connection{}, ErrUnknownAgent
	}
	if !supports(a, agent.MethodAPIKey) {
		return store.Connection{}, ErrMethodUnsupported
	}
	key = strings.TrimSpace(key)
	if len(key) < 8 || len(key) > maxAPIKeyLen || strings.ContainsAny(key, " \t\r\n\x00") {
		return store.Connection{}, ErrInvalidAPIKey
	}
	sealed, err := s.box.Seal([]byte(key), secretAD(userID, name))
	if err != nil {
		return store.Connection{}, err
	}
	c := store.Connection{UserID: userID, Provider: name, Status: store.ConnConnected, Method: agent.MethodAPIKey, Account: maskKey(key), Secret: sealed, UpdatedAt: s.now().UTC()}
	return c, s.store.UpsertConnection(ctx, c)
}

func (s *Service) RemoveAPIKey(ctx context.Context, userID, name string) (store.Connection, error) {
	if _, ok := agent.Get(name); !ok {
		return store.Connection{}, ErrUnknownAgent
	}
	c := store.Connection{UserID: userID, Provider: name, Status: store.ConnDisconnected, UpdatedAt: s.now().UTC()}
	return c, s.store.UpsertConnection(ctx, c)
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

func supports(a agent.Adapter, method string) bool {
	for _, m := range a.Methods() {
		if m == method {
			return true
		}
	}
	return false
}

type Credentials struct {
	Env       []string
	UseAPIKey bool
	Method    string
}

func (s *Service) Credentials(ctx context.Context, userID string, a agent.Adapter) (Credentials, error) {
	c, err := s.store.GetConnection(ctx, userID, a.Name())
	if err != nil || c.Method != agent.MethodAPIKey || c.Secret == "" {
		return Credentials{Method: agent.MethodAccount}, nil
	}
	key, err := s.box.Open(c.Secret, secretAD(userID, a.Name()))
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Env: a.APIKeyEnv(string(key)), UseAPIKey: true, Method: agent.MethodAPIKey}, nil
}

func (s *Service) Probe(ctx context.Context, in store.Instance, a agent.Adapter) store.Connection {
	current := s.Connection(ctx, in.UserID, a.Name())
	if current.Method == agent.MethodAPIKey && current.Secret != "" {
		return current
	}
	if a.StatusArgs() == nil || in.State != store.StateRunning {
		return current
	}
	pctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	res, err := docker.Output(pctx, s.engine, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: a.StatusArgs(), WorkDir: instance.HomeDir}, nil, 64<<10)
	if err != nil {
		s.log.Warn("agent status probe failed", "agent", a.Name(), "instance", in.ID, "error", err)
		return current
	}
	ok, account := a.ParseStatus(res.ExitCode, res.Stdout)
	next := store.Connection{UserID: in.UserID, Provider: a.Name(), Method: agent.MethodAccount, UpdatedAt: s.now().UTC()}
	switch {
	case ok:
		next.Status, next.Account = store.ConnConnected, account
	case current.Status == store.ConnConnected && current.Method == agent.MethodAccount:
		next.Status = store.ConnExpired
	default:
		next.Status = store.ConnSetupRequired
	}
	if next.Status != current.Status || next.Account != current.Account || next.Method != current.Method {
		if err := s.store.UpsertConnection(ctx, next); err != nil {
			s.log.Warn("save connection failed", "error", err)
		}
	}
	return next
}

func (s *Service) MarkAuthFailure(ctx context.Context, userID, name, method string) {
	c := s.Connection(ctx, userID, name)
	if c.Method == "" {
		c.Method = method
	}
	switch {
	case c.Method == agent.MethodAPIKey:
		c.Status, c.Error = store.ConnError, "api key rejected by the provider"
	case c.Status == store.ConnConnected:
		c.Status, c.Error = store.ConnExpired, "authentication failed during a run"
	default:
		c.Status, c.Error = store.ConnSetupRequired, "authentication failed during a run"
	}
	c.UpdatedAt = s.now().UTC()
	_ = s.store.UpsertConnection(ctx, c)
}

func (s *Service) Logout(ctx context.Context, in store.Instance, name string) (store.Connection, error) {
	a, ok := agent.Get(name)
	if !ok {
		return store.Connection{}, ErrUnknownAgent
	}
	if a.LogoutArgs() == nil {
		return store.Connection{}, ErrMethodUnsupported
	}
	release, err := s.inst.Acquire(ctx, in.ID)
	if err != nil {
		return store.Connection{}, err
	}
	defer release()
	pctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	if _, err := docker.Output(pctx, s.engine, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: a.LogoutArgs(), WorkDir: instance.HomeDir}, nil, 64<<10); err != nil {
		return store.Connection{}, err
	}
	c := s.Connection(ctx, in.UserID, name)
	if c.Method != agent.MethodAPIKey {
		c = store.Connection{UserID: in.UserID, Provider: name, Status: store.ConnDisconnected}
	}
	c.UpdatedAt = s.now().UTC()
	return c, s.store.UpsertConnection(ctx, c)
}

type LoginStatus string

const (
	LoginStarting  LoginStatus = "starting"
	LoginWaiting   LoginStatus = "waiting_for_user"
	LoginSucceeded LoginStatus = "succeeded"
	LoginFailed    LoginStatus = "failed"
	LoginCancelled LoginStatus = "cancelled"
)

type Login struct {
	ID         string      `json:"id"`
	InstanceID string      `json:"instance_id"`
	Agent      string      `json:"agent"`
	Status     LoginStatus `json:"status"`
	URL        string      `json:"url,omitempty"`
	Code       string      `json:"code,omitempty"`
	NeedsInput bool        `json:"needs_input"`
	Error      string      `json:"error,omitempty"`
	ExpiresAt  time.Time   `json:"expires_at"`
}

type loginSession struct {
	Login
	mu        sync.Mutex
	proc      docker.Process
	cancel    context.CancelFunc
	inputSent bool
}

func (l *loginSession) snapshot() Login {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := l.Login
	v.NeedsInput = l.NeedsInput && !l.inputSent
	return v
}

func loginKey(instanceID, name string) string { return instanceID + "/" + name }

func (s *Service) StartLogin(ctx context.Context, in store.Instance, name string) (Login, error) {
	a, ok := agent.Get(name)
	if !ok {
		return Login{}, ErrUnknownAgent
	}
	spec := a.Login()
	if spec == nil {
		return Login{}, ErrMethodUnsupported
	}
	key := loginKey(in.ID, name)
	s.mu.Lock()
	if existing, ok := s.logins[key]; ok {
		snap := existing.snapshot()
		if snap.Status == LoginStarting || snap.Status == LoginWaiting {
			s.mu.Unlock()
			return snap, nil
		}
	}
	s.mu.Unlock()

	release, err := s.inst.Acquire(ctx, in.ID)
	if err != nil {
		return Login{}, err
	}
	lctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	id := instance.NewID("login_")
	proc, err := s.engine.Exec(lctx, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: instance.Wrap(id, instance.HomeDir, spec.Args), Tty: spec.Tty, Stdin: spec.NeedsInput, WorkDir: instance.HomeDir, Env: []string{"BROWSER=false", "TERM=dumb", "NO_COLOR=1"}})
	if err != nil {
		cancel()
		release()
		return Login{}, err
	}
	l := &loginSession{Login: Login{ID: id, InstanceID: in.ID, Agent: name, Status: LoginStarting, NeedsInput: spec.NeedsInput, ExpiresAt: s.now().UTC().Add(loginTimeout)}, proc: proc, cancel: cancel}
	s.mu.Lock()
	s.logins[key] = l
	s.mu.Unlock()

	ready := make(chan struct{})
	go s.watchLogin(lctx, in, a, l, release, ready)
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
	case <-ctx.Done():
	}
	return l.snapshot(), nil
}

func (s *Service) watchLogin(ctx context.Context, in store.Instance, a agent.Adapter, l *loginSession, release func(), ready chan struct{}) {
	defer release()
	defer l.cancel()
	defer l.proc.Close()
	streamDone := make(chan struct{})
	defer func() {
		if ctx.Err() == nil {
			close(streamDone)
		}
	}()
	go func() {
		select {
		case <-streamDone:
			return
		case <-ctx.Done():
		}
		kctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = instance.Kill(kctx, s.engine, in.ID, l.ID)
	}()
	var readyOnce sync.Once
	signal := func() { readyOnce.Do(func() { close(ready) }) }
	var buf strings.Builder
	chunk := make([]byte, 4096)
	for {
		n, err := l.proc.Stdout().Read(chunk)
		if n > 0 && buf.Len() < maxLoginOutput {
			buf.Write(chunk[:n])
			text := buf.String()
			l.mu.Lock()
			if l.URL == "" {
				l.URL = agent.FindURL(text, "oauth", "authorize", "device", "auth.openai.com", "claude.ai", "claude.com")
			}
			if l.Code == "" && !l.NeedsInput {
				l.Code = agent.FindDeviceCode(text)
			}
			if l.URL != "" && l.Status == LoginStarting && (l.NeedsInput || l.Code != "") {
				l.Status = LoginWaiting
				signal()
			}
			l.mu.Unlock()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("login stream closed", "error", err)
			}
			break
		}
	}
	code, _ := l.proc.Wait(context.Background())
	probeCtx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	conn := s.Probe(probeCtx, in, a)
	l.mu.Lock()
	switch {
	case l.Status == LoginCancelled:
	case conn.Status == store.ConnConnected:
		l.Status = LoginSucceeded
	default:
		l.Status = LoginFailed
		if ctx.Err() != nil {
			l.Error = "login_timeout"
		} else if code != 0 {
			l.Error = "login_failed"
		} else {
			l.Error = "not_authenticated"
		}
	}
	l.mu.Unlock()
	signal()
}

func (s *Service) GetLogin(instanceID, name string) (Login, error) {
	s.mu.Lock()
	l, ok := s.logins[loginKey(instanceID, name)]
	s.mu.Unlock()
	if !ok {
		return Login{}, ErrLoginNotFound
	}
	return l.snapshot(), nil
}

func (s *Service) SendLoginInput(instanceID, name, code string) (Login, error) {
	s.mu.Lock()
	l, ok := s.logins[loginKey(instanceID, name)]
	s.mu.Unlock()
	if !ok {
		return Login{}, ErrLoginNotFound
	}
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 2048 || strings.IndexFunc(code, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return Login{}, ErrInvalidInput
	}
	l.mu.Lock()
	if !l.NeedsInput || l.inputSent || l.Status != LoginWaiting {
		l.mu.Unlock()
		return Login{}, ErrLoginNoInput
	}
	l.inputSent = true
	l.mu.Unlock()
	if _, err := l.proc.Write([]byte(code + "\r")); err != nil {
		return Login{}, err
	}
	return l.snapshot(), nil
}

func (s *Service) CancelLogin(instanceID, name string) (Login, error) {
	s.mu.Lock()
	l, ok := s.logins[loginKey(instanceID, name)]
	s.mu.Unlock()
	if !ok {
		return Login{}, ErrLoginNotFound
	}
	l.mu.Lock()
	if l.Status == LoginStarting || l.Status == LoginWaiting {
		l.Status = LoginCancelled
	}
	l.mu.Unlock()
	l.cancel()
	return l.snapshot(), nil
}

func (s *Service) CancelInstance(instanceID string) {
	s.mu.Lock()
	var list []*loginSession
	for k, l := range s.logins {
		if strings.HasPrefix(k, instanceID+"/") {
			list = append(list, l)
			delete(s.logins, k)
		}
	}
	s.mu.Unlock()
	for _, l := range list {
		l.mu.Lock()
		if l.Status == LoginStarting || l.Status == LoginWaiting {
			l.Status = LoginCancelled
		}
		l.mu.Unlock()
		l.cancel()
	}
}
