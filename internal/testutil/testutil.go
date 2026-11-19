package testutil

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/docker/fakedocker"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/secret"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const Image = "aag-runtime:test"

type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock() *Clock { return &Clock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func Store(t testing.TB) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func Box(t testing.TB) *secret.Box {
	t.Helper()
	b, err := secret.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func Settings(maxActive int) instance.Settings {
	return instance.Settings{Image: Image, Network: "aag-test", Memory: 1 << 30, CPUs: 1, Pids: 256, MaxActive: maxActive, Retention: 30}
}

type Env struct {
	Store     *store.Store
	Dir       string
	Engine    *fakedocker.Engine
	Clock     *Clock
	Instances *instance.Manager
	Box       *secret.Box
}

func NewEnv(t testing.TB, maxActive int) *Env {
	t.Helper()
	s, dir := Store(t)
	e := fakedocker.New(Image)
	clock := NewClock()
	return &Env{Store: s, Dir: dir, Engine: e, Clock: clock, Box: Box(t), Instances: instance.NewManager(s, e, Settings(maxActive), clock.Now, Logger())}
}

func (env *Env) Running(t testing.TB, user string) store.Instance {
	t.Helper()
	in, _, err := env.Instances.Create(context.Background(), user, "")
	if err != nil {
		t.Fatal(err)
	}
	in, err = env.Instances.Start(context.Background(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	return in
}
