package idle

import (
	"context"
	"log/slog"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

type Stopper interface {
	StopIfIdle(ctx context.Context, id string, timeout time.Duration) (bool, error)
}

type Lister interface {
	ListInstances(ctx context.Context, userID string) ([]store.Instance, error)
}

type Manager struct {
	list     Lister
	stopper  Stopper
	timeout  time.Duration
	interval time.Duration
	log      *slog.Logger
}

func New(list Lister, stopper Stopper, timeout, interval time.Duration, log *slog.Logger) *Manager {
	return &Manager{list: list, stopper: stopper, timeout: timeout, interval: interval, log: log}
}

func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx)
		}
	}
}

func (m *Manager) Check(ctx context.Context) int {
	instances, err := m.list.ListInstances(ctx, "")
	if err != nil {
		m.log.Warn("idle check: list instances", "error", err)
		return 0
	}
	stopped := 0
	for _, in := range instances {
		if in.State != store.StateRunning {
			continue
		}
		ok, err := m.stopper.StopIfIdle(ctx, in.ID, m.timeout)
		if err != nil {
			m.log.Warn("idle stop failed", "instance", in.ID, "error", err)
			continue
		}
		if ok {
			stopped++
			m.log.Info("instance stopped after inactivity", "instance", in.ID)
		}
	}
	return stopped
}
