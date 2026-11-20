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

var loginProbeInterval = 500 * time.Millisecond

var (
	ErrUnknownAgent      = errors.New("unknown agent")
	ErrMethodUnsupported = errors.New("method not supported by this agent")
	ErrInvalidAPIKey     = errors.New("invalid api key")
	ErrInvalidInput      = errors.New("invalid login input")
	ErrLoginNotFound     = errors.New("login session not found")
	ErrLoginNoInput      = errors.New("this login does not accept input")
	ErrLoginInProgress   = errors.New("agent login is in progress")
)

type Service struct {
	store    *store.Store
	box      *secret.Box
	inst     *instance.Manager
	engine   docker.Engine
	now      func() time.Time
	log      *slog.Logger
	mu       sync.Mutex
	logins   map[string]*loginSession
	changing map[string]bool
}

func New(s *store.Store, box *secret.Box, inst *instance.Manager, engine docker.Engine, now func() time.Time, log *slog.Logger) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: s, box: box, inst: inst, engine: engine, now: now, log: log, logins: map[string]*loginSession{}, changing: map[string]bool{}}
}

func secretAD(instanceID, provider string) string { return "connection:" + provider + ":" + instanceID }

func (s *Service) Box() *secret.Box { return s.box }

func (s *Service) Connection(ctx context.Context, instanceID, provider string) store.Connection {
	c, err := s.store.GetConnection(ctx, instanceID, provider)
	if err != nil {
		return store.Connection{InstanceID: instanceID, Provider: provider, Status: store.ConnSetupRequired}
	}
	return c
}

func (s *Service) SetAPIKey(ctx context.Context, instanceID, name, key string) (store.Connection, error) {
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
	finish, err := s.beginChange(loginKey(instanceID, name))
	if err != nil {
		return store.Connection{}, err
	}
	defer finish()
	sealed, err := s.box.Seal([]byte(key), secretAD(instanceID, name))
	if err != nil {
		return store.Connection{}, err
	}
	c := store.Connection{InstanceID: instanceID, Provider: name, Status: store.ConnConnected, Method: agent.MethodAPIKey, Account: maskKey(key), Secret: sealed, UpdatedAt: s.now().UTC()}
	return c, s.store.UpsertConnection(ctx, c)
}

func (s *Service) RemoveAPIKey(ctx context.Context, instanceID, name string) (store.Connection, error) {
	if _, ok := agent.Get(name); !ok {
		return store.Connection{}, ErrUnknownAgent
	}
	finish, err := s.beginChange(loginKey(instanceID, name))
	if err != nil {
		return store.Connection{}, err
	}
	defer finish()
	c := store.Connection{InstanceID: instanceID, Provider: name, Status: store.ConnDisconnected, UpdatedAt: s.now().UTC()}
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

func mergeEnv(base, extra []string) []string {
	out := append([]string(nil), base...)
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		replaced := false
		for i, cur := range out {
			if ck, _, _ := strings.Cut(cur, "="); ck == k {
				out[i] = kv
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

type Credentials struct {
	Env       []string
	UseAPIKey bool
	Method    string
}

func (s *Service) Credentials(ctx context.Context, instanceID string, a agent.Adapter) (Credentials, error) {
	c, err := s.store.GetConnection(ctx, instanceID, a.Name())
	if err != nil || c.Method != agent.MethodAPIKey || c.Secret == "" {
		return Credentials{Method: agent.MethodAccount}, nil
	}
	key, err := s.box.Open(c.Secret, secretAD(instanceID, a.Name()))
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Env: a.APIKeyEnv(string(key)), UseAPIKey: true, Method: agent.MethodAPIKey}, nil
}

func (s *Service) Probe(ctx context.Context, in store.Instance, a agent.Adapter) store.Connection {
	return s.probe(ctx, in, a, nil)
}

func (s *Service) probe(ctx context.Context, in store.Instance, a agent.Adapter, login *loginSession) store.Connection {
	key := loginKey(in.ID, a.Name())
	s.mu.Lock()
	current := s.Connection(ctx, in.ID, a.Name())
	blocked := s.changing[key] || (s.logins[key] != login && s.logins[key].active())
	s.mu.Unlock()
	if blocked {
		return current
	}
	if login == nil && current.Method == agent.MethodAPIKey && current.Secret != "" {
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
	if !ok && res.Stderr != "" {
		ok, account = a.ParseStatus(res.ExitCode, res.Stderr)
	}
	next := store.Connection{InstanceID: in.ID, Provider: a.Name(), Method: agent.MethodAccount, UpdatedAt: s.now().UTC()}
	switch {
	case ok:
		next.Status, next.Account = store.ConnConnected, account
	case current.Status == store.ConnConnected && current.Method == agent.MethodAccount:
		next.Status = store.ConnExpired
	default:
		if login != nil && current.Method == agent.MethodAPIKey {
			return current
		}
		next.Status = store.ConnSetupRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := s.Connection(ctx, in.ID, a.Name())
	if s.changing[key] || (s.logins[key] != login && s.logins[key].active()) || latest != current {
		return latest
	}
	if next.Status != current.Status || next.Account != current.Account || next.Method != current.Method {
		if err := s.store.UpsertConnection(ctx, next); err != nil {
			s.log.Warn("save connection failed")
			return current
		}
	}
	return next
}

func (s *Service) MarkAuthFailure(ctx context.Context, instanceID, name, method string) {
	c := s.Connection(ctx, instanceID, name)
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
	spec := a.Logout()
	if spec == nil {
		return store.Connection{}, ErrMethodUnsupported
	}
	finish, err := s.beginChange(loginKey(in.ID, name))
	if err != nil {
		return store.Connection{}, err
	}
	defer finish()
	release, err := s.inst.Acquire(ctx, in.ID)
	if err != nil {
		return store.Connection{}, err
	}
	defer release()
	pctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	if !spec.Tty {
		if _, err := docker.Output(pctx, s.engine, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: spec.Args, WorkDir: instance.HomeDir, Env: spec.Env}, nil, 64<<10); err != nil {
			return store.Connection{}, err
		}
	} else if err := s.ttyLogout(pctx, in, spec); err != nil {
		return store.Connection{}, err
	}
	c := s.Connection(ctx, in.ID, name)
	if c.Method != agent.MethodAPIKey {
		c = store.Connection{InstanceID: in.ID, Provider: name, Status: store.ConnDisconnected}
	}
	c.UpdatedAt = s.now().UTC()
	return c, s.store.UpsertConnection(ctx, c)
}

func (s *Service) ttyLogout(ctx context.Context, in store.Instance, spec *agent.Logout) error {
	id := instance.NewID("logout_")
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	proc, err := s.engine.Exec(lctx, docker.ExecSpec{
		Container: instance.ContainerName(in.ID),
		Cmd:       instance.Wrap(id, instance.HomeDir, spec.Args),
		Tty:       true,
		Stdin:     true,
		WorkDir:   instance.HomeDir,
		Env:       mergeEnv([]string{"BROWSER=false", "TERM=dumb", "NO_COLOR=1"}, spec.Env),
	})
	if err != nil {
		return err
	}
	defer proc.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
			return
		case <-lctx.Done():
		}
		kctx, kcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer kcancel()
		_ = instance.Kill(kctx, s.engine, in.ID, id)
	}()
	var buf strings.Builder
	chunk := make([]byte, 4096)
	for {
		n, rerr := proc.Stdout().Read(chunk)
		if n > 0 && buf.Len() < maxLoginOutput {
			buf.Write(chunk[:n])
			if len(spec.Done) > 0 && agent.ContainsAny(agent.StripANSI(buf.String()), spec.Done...) {
				cancel()
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	cancel()
	_, _ = proc.Wait(context.Background())
	return nil
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
	mu                sync.Mutex
	proc              docker.Process
	cancel            context.CancelFunc
	inputSent         bool
	inputNotify       chan struct{}
	done              chan struct{}
	cancelledByUser   bool
	failedByOutput    bool
	succeededByOutput bool
}

func (l *loginSession) snapshot() Login {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := l.Login
	v.NeedsInput = l.NeedsInput && !l.inputSent
	return v
}

func (l *loginSession) active() bool {
	if l == nil {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

func loginKey(instanceID, name string) string { return instanceID + "/" + name }

func (s *Service) beginChange(key string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changing[key] || s.logins[key].active() {
		return nil, ErrLoginInProgress
	}
	s.changing[key] = true
	return func() {
		s.mu.Lock()
		delete(s.changing, key)
		s.mu.Unlock()
	}, nil
}

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
		if existing.active() {
			s.mu.Unlock()
			return snap, nil
		}
	}
	s.mu.Unlock()

	finish, err := s.beginChange(key)
	if err != nil {
		return Login{}, err
	}

	release, err := s.inst.Acquire(ctx, in.ID)
	if err != nil {
		finish()
		return Login{}, err
	}
	lctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	id := instance.NewID("login_")
	env := mergeEnv([]string{"BROWSER=false", "TERM=dumb", "NO_COLOR=1"}, spec.Env)
	proc, err := s.engine.Exec(lctx, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: instance.Wrap(id, instance.HomeDir, spec.Args), Tty: spec.Tty, Stdin: spec.NeedsInput || len(spec.AutoSend) > 0, WorkDir: instance.HomeDir, Env: env})
	if err != nil {
		cancel()
		release()
		finish()
		return Login{}, err
	}
	l := &loginSession{
		Login:       Login{ID: id, InstanceID: in.ID, Agent: name, Status: LoginStarting, NeedsInput: spec.NeedsInput, ExpiresAt: s.now().UTC().Add(loginTimeout)},
		proc:        proc,
		cancel:      cancel,
		inputNotify: make(chan struct{}),
		done:        make(chan struct{}),
	}
	s.mu.Lock()
	s.logins[key] = l
	s.mu.Unlock()
	finish()

	ready := make(chan struct{})
	go s.watchLogin(lctx, in, a, spec, l, release, ready)
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
	case <-ctx.Done():
	}
	return l.snapshot(), nil
}

func (s *Service) watchLogin(ctx context.Context, in store.Instance, a agent.Adapter, spec *agent.Login, l *loginSession, release func(), ready chan struct{}) {
	defer close(l.done)
	defer release()
	defer l.cancel()
	defer l.proc.Close()
	streamDone := make(chan struct{})
	var closeStream sync.Once
	stopWatchers := func() { closeStream.Do(func() { close(streamDone) }) }
	defer stopWatchers()
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
	if len(spec.Success) > 0 && a.StatusArgs() != nil {
		go func() {
			select {
			case <-l.inputNotify:
			case <-streamDone:
				return
			case <-ctx.Done():
				return
			}
			ticker := time.NewTicker(loginProbeInterval)
			defer ticker.Stop()
			for {
				select {
				case <-streamDone:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					pctx, pcancel := context.WithTimeout(ctx, statusTimeout)
					conn := s.probe(pctx, in, a, l)
					pcancel()
					if conn.Status == store.ConnConnected && conn.Method == agent.MethodAccount {
						l.mu.Lock()
						l.succeededByOutput = true
						l.mu.Unlock()
						l.cancel()
						return
					}
				}
			}
		}()
	}
	var readyOnce sync.Once
	signal := func() { readyOnce.Do(func() { close(ready) }) }
	var buf strings.Builder
	autoSent := make([]bool, len(spec.AutoSend))
	chunk := make([]byte, 4096)
	for {
		n, err := l.proc.Stdout().Read(chunk)
		if n > 0 && buf.Len() < maxLoginOutput {
			buf.Write(chunk[:n])
			text := buf.String()
			clean := agent.StripANSI(text)
			for i, rule := range spec.AutoSend {
				if !autoSent[i] && agent.ContainsAny(clean, rule.Match) {
					autoSent[i] = true
					_, _ = l.proc.Write([]byte(rule.Input))
				}
			}
			l.mu.Lock()
			if l.URL == "" && l.Status == LoginStarting {
				l.URL = spec.FindAuthorizeURL(text)
			}
			if l.Code == "" && !l.NeedsInput && l.Status == LoginStarting {
				l.Code = agent.FindDeviceCode(text)
			}
			if l.URL != "" && l.Status == LoginStarting && (l.NeedsInput || l.Code != "") {
				l.Status = LoginWaiting
				signal()
			}
			shouldStop := false
			if (l.inputSent || !l.NeedsInput) && len(spec.Failure) > 0 && agent.ContainsAny(clean, spec.Failure...) {
				l.failedByOutput = true
				shouldStop = true
			} else if len(spec.Success) > 0 && agent.ContainsAny(clean, spec.Success...) {
				l.succeededByOutput = true
				shouldStop = true
			}
			l.mu.Unlock()
			if shouldStop {
				l.cancel()
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("login stream closed", "agent", a.Name())
			}
			break
		}
	}
	code, _ := l.proc.Wait(context.Background())
	stopWatchers()
	probeCtx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	l.mu.Lock()
	cancelled := l.cancelledByUser || l.Status == LoginCancelled
	failedOut := l.failedByOutput
	succeededOut := l.succeededByOutput
	l.mu.Unlock()

	var conn store.Connection
	if !cancelled && !failedOut {
		conn = s.probe(probeCtx, in, a, l)
	}
	l.mu.Lock()
	switch {
	case cancelled:
		l.Status = LoginCancelled
	case failedOut:
		l.Status = LoginFailed
		l.Error = "login_failed"
	case conn.Status == store.ConnConnected && conn.Method == agent.MethodAccount:
		l.Status = LoginSucceeded
	default:
		l.Status = LoginFailed
		if ctx.Err() != nil && !succeededOut {
			l.Error = "login_timeout"
		} else if code != 0 {
			l.Error = "login_failed"
		} else {
			l.Error = "not_authenticated"
		}
	}
	l.URL, l.Code = "", ""
	result := l.Status
	reason := l.Error
	l.mu.Unlock()
	s.log.Info("agent login finished", "agent", a.Name(), "status", result, "reason", reason, "exit_code", code)
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
	if _, err := l.proc.Write([]byte(code + "\r")); err != nil {
		l.mu.Unlock()
		return Login{}, err
	}
	l.inputSent = true
	l.URL = ""
	close(l.inputNotify)
	l.mu.Unlock()
	select {
	case <-l.done:
	case <-time.After(2 * time.Second):
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
	if l.inputSent && (l.Status == LoginStarting || l.Status == LoginWaiting) {
		l.mu.Unlock()
		return Login{}, ErrLoginInProgress
	}
	if l.Status == LoginStarting || l.Status == LoginWaiting {
		l.Status = LoginCancelled
		l.cancelledByUser = true
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
			l.cancelledByUser = true
		}
		l.mu.Unlock()
		l.cancel()
	}
}
