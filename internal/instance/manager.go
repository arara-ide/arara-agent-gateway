package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/docker"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const (
	HomeDir      = "/home/agent"
	WorkDir      = HomeDir + "/work"
	labelID      = "aag.instance"
	stopTimeout  = 10 * time.Second
	agentUser    = "1000:1000"
	tmpfsOptions = "rw,nosuid,nodev,size=1g,uid=1000,gid=1000,mode=1777"
)

var (
	ErrNotRunning   = errors.New("instance is not running")
	ErrCapacity     = errors.New("maximum number of active instances reached")
	ErrImageMissing = errors.New("runtime image is not available on the docker host")
)

type Settings struct {
	Image     string
	Network   string
	Runtime   string
	Memory    int64
	CPUs      float64
	Pids      int64
	MaxActive int
	Retention int
}

type Manager struct {
	store      *store.Store
	engine     docker.Engine
	settings   Settings
	now        func() time.Time
	log        *slog.Logger
	locks      sync.Map
	startMu    sync.Mutex
	mu         sync.Mutex
	busy       map[string]int
	BeforeStop func(ctx context.Context, id string)
}

func NewManager(s *store.Store, e docker.Engine, settings Settings, now func() time.Time, log *slog.Logger) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{store: s, engine: e, settings: settings, now: now, log: log, busy: map[string]int{}}
}

func NewID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func ContainerName(id string) string { return "aag-" + id }
func VolumeName(id string) string    { return "aag-" + id }

func (m *Manager) lock(id string) func() {
	v, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (m *Manager) Create(ctx context.Context, name string) (store.Instance, error) {
	now := m.now().UTC()
	in := store.Instance{ID: NewID("inst_"), Name: name, State: store.StateStopped, CreatedAt: now, LastActivityAt: now}
	return m.store.CreateInstance(ctx, in)
}

func (m *Manager) Get(ctx context.Context, id string) (store.Instance, error) {
	return m.store.GetInstance(ctx, id)
}

func (m *Manager) Busy(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy[id]
}

func (m *Manager) Start(ctx context.Context, id string) (store.Instance, error) {
	unlock := m.lock(id)
	defer unlock()
	in, err := m.store.GetInstance(ctx, id)
	if err != nil {
		return store.Instance{}, err
	}
	if in.State == store.StateRunning {
		info, err := m.engine.InspectContainer(ctx, ContainerName(id))
		if err == nil && info.Running {
			return in, nil
		}
	}
	if err := m.reserveSlot(ctx, id); err != nil {
		return in, err
	}
	if err := m.boot(ctx, in); err != nil {
		msg := err.Error()
		_ = m.store.SetInstanceState(context.WithoutCancel(ctx), id, store.StateError, "", msg)
		in.State, in.LastError = store.StateError, msg
		return in, err
	}
	now := m.now().UTC()
	if err := m.store.SetInstanceState(ctx, id, store.StateRunning, ContainerName(id), ""); err != nil {
		return in, err
	}
	if err := m.store.TouchInstance(ctx, id, now); err != nil {
		return in, err
	}
	m.prune(id)
	return m.store.GetInstance(ctx, id)
}

func (m *Manager) prune(id string) {
	if m.settings.Retention <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := []string{"aag-prune", strconv.Itoa(m.settings.Retention)}
	if _, err := docker.Output(ctx, m.engine, docker.ExecSpec{Container: ContainerName(id), Cmd: cmd}, nil, 4096); err != nil {
		m.log.Warn("session retention cleanup failed", "instance", id, "error", err)
	}
}

func (m *Manager) reserveSlot(ctx context.Context, id string) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	active, err := m.store.CountInstancesInStates(ctx, store.StateRunning, store.StateStarting)
	if err != nil {
		return err
	}
	if active >= m.settings.MaxActive {
		return ErrCapacity
	}
	return m.store.SetInstanceState(ctx, id, store.StateStarting, "", "")
}

func (m *Manager) boot(ctx context.Context, in store.Instance) error {
	ok, err := m.engine.ImageExists(ctx, m.settings.Image)
	if err != nil {
		return err
	}
	if !ok {
		return ErrImageMissing
	}
	if err := m.engine.EnsureNetwork(ctx, m.settings.Network); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if err := m.engine.EnsureVolume(ctx, VolumeName(in.ID), map[string]string{labelID: in.ID}); err != nil {
		return fmt.Errorf("volume: %w", err)
	}
	name := ContainerName(in.ID)
	if err := m.engine.RemoveContainer(ctx, name); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	_, err = m.engine.CreateContainer(ctx, docker.ContainerSpec{
		Name:     name,
		Image:    m.settings.Image,
		User:     agentUser,
		Env:      []string{"HOME=" + HomeDir, "AGY_CLI_DISABLE_AUTO_UPDATE=true", "DISABLE_AUTOUPDATER=1"},
		Labels:   map[string]string{labelID: in.ID},
		Cmd:      []string{"sleep", "infinity"},
		Mounts:   []docker.Mount{{Volume: VolumeName(in.ID), Target: HomeDir}},
		Memory:   m.settings.Memory,
		NanoCPUs: int64(m.settings.CPUs * 1e9),
		Pids:     m.settings.Pids,
		Network:  m.settings.Network,
		Runtime:  m.settings.Runtime,
		Tmpfs:    map[string]string{"/tmp": tmpfsOptions},
	})
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := m.engine.StartContainer(ctx, name); err != nil {
		_ = m.engine.RemoveContainer(context.WithoutCancel(ctx), name)
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context, id string) (store.Instance, error) {
	unlock := m.lock(id)
	defer unlock()
	return m.stopLocked(ctx, id)
}

func (m *Manager) stopLocked(ctx context.Context, id string) (store.Instance, error) {
	in, err := m.store.GetInstance(ctx, id)
	if err != nil {
		return store.Instance{}, err
	}
	if in.State == store.StateStopped {
		return in, nil
	}
	if m.BeforeStop != nil {
		m.BeforeStop(ctx, id)
	}
	if err := m.store.SetInstanceState(ctx, id, store.StateStopping, in.ContainerID, ""); err != nil {
		return in, err
	}
	name := ContainerName(id)
	if err := m.engine.StopContainer(ctx, name, stopTimeout); err != nil && !errors.Is(err, docker.ErrNotFound) {
		m.log.Warn("stop container failed", "instance", id, "error", err)
	}
	if err := m.engine.RemoveContainer(ctx, name); err != nil {
		_ = m.store.SetInstanceState(ctx, id, store.StateError, "", err.Error())
		return in, err
	}
	if err := m.store.SetInstanceState(ctx, id, store.StateStopped, "", ""); err != nil {
		return in, err
	}
	return m.store.GetInstance(ctx, id)
}

func (m *Manager) StopIfIdle(ctx context.Context, id string, timeout time.Duration) (bool, error) {
	unlock := m.lock(id)
	defer unlock()
	in, err := m.store.GetInstance(ctx, id)
	if err != nil {
		return false, err
	}
	if in.State != store.StateRunning || m.Busy(id) > 0 {
		return false, nil
	}
	if m.now().Sub(in.LastActivityAt) < timeout {
		return false, nil
	}
	if _, err := m.stopLocked(ctx, id); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	unlock := m.lock(id)
	defer unlock()
	if _, err := m.store.GetInstance(ctx, id); err != nil {
		return err
	}
	if m.BeforeStop != nil {
		m.BeforeStop(ctx, id)
	}
	if err := m.engine.RemoveContainer(ctx, ContainerName(id)); err != nil {
		return err
	}
	if err := m.engine.RemoveVolume(ctx, VolumeName(id)); err != nil {
		return err
	}
	if err := m.store.DeleteInstance(ctx, id); err != nil {
		return err
	}
	m.locks.Delete(id)
	return nil
}

func (m *Manager) Acquire(ctx context.Context, id string) (func(), error) {
	unlock := m.lock(id)
	defer unlock()
	in, err := m.store.GetInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.State != store.StateRunning {
		return nil, ErrNotRunning
	}
	m.mu.Lock()
	m.busy[id]++
	m.mu.Unlock()
	_ = m.store.TouchInstance(ctx, id, m.now().UTC())
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.busy[id] > 1 {
				m.busy[id]--
			} else {
				delete(m.busy, id)
			}
			m.mu.Unlock()
			_ = m.store.TouchInstance(context.Background(), id, m.now().UTC())
		})
	}, nil
}

func (m *Manager) Touch(ctx context.Context, id string) error {
	return m.store.TouchInstance(ctx, id, m.now().UTC())
}

func (m *Manager) Reconcile(ctx context.Context) error {
	if n, err := m.store.InterruptRunningRuns(ctx, m.now().UTC()); err != nil {
		return err
	} else if n > 0 {
		m.log.Info("marked interrupted runs", "count", n)
	}
	containers, err := m.engine.ListContainers(ctx, labelID)
	if err != nil {
		return err
	}
	running := map[string]bool{}
	for _, c := range containers {
		id := c.Labels[labelID]
		if _, err := m.store.GetInstance(ctx, id); errors.Is(err, store.ErrNotFound) {
			m.log.Warn("removing orphan container", "container", c.Name)
			if err := m.engine.RemoveContainer(ctx, c.ID); err != nil {
				return err
			}
			continue
		}
		running[id] = c.Running
	}
	instances, err := m.store.ListInstances(ctx)
	if err != nil {
		return err
	}
	now := m.now().UTC()
	for _, in := range instances {
		if running[in.ID] {
			if err := m.store.SetInstanceState(ctx, in.ID, store.StateRunning, ContainerName(in.ID), ""); err != nil {
				return err
			}
			if err := m.store.TouchInstance(ctx, in.ID, now); err != nil {
				return err
			}
			if _, err := docker.Output(ctx, m.engine, docker.ExecSpec{Container: ContainerName(in.ID), Cmd: []string{"aag-kill", "all"}}, nil, 4096); err != nil {
				m.log.Warn("orphan process cleanup failed", "instance", in.ID, "error", err)
			}
			continue
		}
		if in.State != store.StateStopped {
			if _, found := running[in.ID]; found {
				if err := m.engine.RemoveContainer(ctx, ContainerName(in.ID)); err != nil {
					return err
				}
			}
			if err := m.store.SetInstanceState(ctx, in.ID, store.StateStopped, "", ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func Wrap(procID, dir string, args []string) []string {
	return append([]string{"aag-run", procID, dir}, args...)
}

func Kill(ctx context.Context, e docker.Engine, instanceID, procID string) error {
	_, err := docker.Output(ctx, e, docker.ExecSpec{Container: ContainerName(instanceID), Cmd: []string{"aag-kill", procID}}, nil, 4096)
	return err
}
