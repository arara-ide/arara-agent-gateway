package fakedocker

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/docker"
)

type Exec struct {
	Spec   docker.ExecSpec
	ProcID string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Killed <-chan struct{}
}

type Handler func(e *Exec) int

type Container struct {
	Spec    docker.ContainerSpec
	Running bool
}

type Engine struct {
	mu         sync.Mutex
	Images     map[string]bool
	Containers map[string]*Container
	Volumes    map[string]bool
	Networks   map[string]bool
	Handler    Handler
	Execs      []docker.ExecSpec
	kills      map[string]chan struct{}
	PingErr    error
	stats      map[string]docker.Stats
	sizes      map[string]int64
}

func New(images ...string) *Engine {
	e := &Engine{Images: map[string]bool{}, Containers: map[string]*Container{}, Volumes: map[string]bool{}, Networks: map[string]bool{}, kills: map[string]chan struct{}{}}
	for _, i := range images {
		e.Images[i] = true
	}
	return e
}

func (e *Engine) SetHandler(h Handler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Handler = h
}

func (e *Engine) ExecLog() []docker.ExecSpec {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]docker.ExecSpec(nil), e.Execs...)
}

func (e *Engine) Container(name string) (Container, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.Containers[name]
	if !ok {
		return Container{}, false
	}
	return *c, true
}

func (e *Engine) HasVolume(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Volumes[name]
}

func (e *Engine) Ping(context.Context) error { return e.PingErr }

func (e *Engine) ImageExists(_ context.Context, ref string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Images[ref], nil
}

func (e *Engine) EnsureNetwork(_ context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Networks[name] = true
	return nil
}

func (e *Engine) EnsureVolume(_ context.Context, name string, _ map[string]string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Volumes[name] = true
	return nil
}

func (e *Engine) RemoveVolume(_ context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.Volumes, name)
	return nil
}

func (e *Engine) CreateContainer(_ context.Context, spec docker.ContainerSpec) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.Containers[spec.Name]; ok {
		return "", errors.New("conflict: container name in use")
	}
	e.Containers[spec.Name] = &Container{Spec: spec}
	return spec.Name, nil
}

func (e *Engine) StartContainer(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.Containers[id]
	if !ok {
		return docker.ErrNotFound
	}
	c.Running = true
	return nil
}

func (e *Engine) StopContainer(_ context.Context, id string, _ time.Duration) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.Containers[id]
	if !ok {
		return docker.ErrNotFound
	}
	c.Running = false
	e.killAllLocked()
	return nil
}

func (e *Engine) RemoveContainer(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.Containers, id)
	return nil
}

func (e *Engine) InspectContainer(_ context.Context, id string) (docker.ContainerInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.Containers[id]
	if !ok {
		return docker.ContainerInfo{}, docker.ErrNotFound
	}
	return docker.ContainerInfo{ID: id, Name: "/" + id, Running: c.Running, Labels: c.Spec.Labels}, nil
}

func (e *Engine) ListContainers(_ context.Context, label string) ([]docker.ContainerInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []docker.ContainerInfo
	for name, c := range e.Containers {
		if _, ok := c.Spec.Labels[label]; ok {
			out = append(out, docker.ContainerInfo{ID: name, Name: "/" + name, Running: c.Running, Labels: c.Spec.Labels})
		}
	}
	return out, nil
}

func (e *Engine) killAllLocked() {
	for id, ch := range e.kills {
		close(ch)
		delete(e.kills, id)
	}
}

func (e *Engine) Exec(ctx context.Context, spec docker.ExecSpec) (docker.Process, error) {
	e.mu.Lock()
	c, ok := e.Containers[spec.Container]
	if !ok || !c.Running {
		e.mu.Unlock()
		return nil, errors.New("container is not running")
	}
	e.Execs = append(e.Execs, spec)
	ex := &Exec{Spec: spec}
	if len(spec.Cmd) > 0 && spec.Cmd[0] == "aag-kill" {
		target := ""
		if len(spec.Cmd) > 1 {
			target = spec.Cmd[1]
		}
		if target == "all" {
			e.killAllLocked()
		} else if ch, ok := e.kills[target]; ok {
			close(ch)
			delete(e.kills, target)
		}
		e.mu.Unlock()
		return newProcess(ctx, ex, func(*Exec) int { return 0 }), nil
	}
	killed := make(chan struct{})
	if len(spec.Cmd) > 3 && spec.Cmd[0] == "aag-run" {
		ex.ProcID = spec.Cmd[1]
		ex.Spec.WorkDir = spec.Cmd[2]
		ex.Spec.Cmd = spec.Cmd[3:]
		e.kills[ex.ProcID] = killed
	}
	ex.Killed = killed
	h := e.Handler
	e.mu.Unlock()
	if h == nil {
		h = func(*Exec) int { return 0 }
	}
	return newProcess(ctx, ex, h), nil
}

type process struct {
	stdout   *io.PipeReader
	stdinW   *io.PipeWriter
	stderr   docker.Tail
	done     chan struct{}
	exitCode int
}

func newProcess(ctx context.Context, ex *Exec, h Handler) *process {
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	p := &process{stdout: outR, stdinW: inW, done: make(chan struct{})}
	ex.Stdin, ex.Stdout, ex.Stderr = inR, outW, &p.stderr
	if !ex.Spec.Stdin {
		inW.Close()
	}
	go func() {
		p.exitCode = h(ex)
		inR.Close()
		outW.Close()
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			outR.CloseWithError(ctx.Err())
		case <-p.done:
		}
	}()
	return p
}

func (p *process) Stdout() io.Reader           { return p.stdout }
func (p *process) StderrTail() string          { return p.stderr.String() }
func (p *process) Write(b []byte) (int, error) { return p.stdinW.Write(b) }
func (p *process) CloseStdin() error           { return p.stdinW.Close() }
func (p *process) Close() error {
	p.stdinW.Close()
	return p.stdout.Close()
}

func (p *process) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.exitCode, nil
	case <-ctx.Done():
		select {
		case <-p.done:
			return p.exitCode, ctx.Err()
		case <-time.After(5 * time.Second):
			return -1, ctx.Err()
		}
	}
}

func ReadAll(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}

func Lines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

func (e *Engine) SetStats(name string, s docker.Stats) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stats == nil {
		e.stats = map[string]docker.Stats{}
	}
	e.stats[name] = s
}

func (e *Engine) SetVolumeSize(name string, size int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sizes == nil {
		e.sizes = map[string]int64{}
	}
	e.sizes[name] = size
}

func (e *Engine) ContainerStats(_ context.Context, id string) (docker.Stats, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.Containers[id]
	if !ok || !c.Running {
		return docker.Stats{}, docker.ErrNotFound
	}
	return e.stats[id], nil
}

func (e *Engine) VolumeSizes(context.Context) (map[string]int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]int64{}
	for name := range e.Volumes {
		out[name] = e.sizes[name]
	}
	return out, nil
}
