package docker

import (
	"context"
	"io"
	"time"
)

type Mount struct {
	Volume string
	Target string
}

type ContainerSpec struct {
	Name     string
	Image    string
	User     string
	Env      []string
	Labels   map[string]string
	Cmd      []string
	Mounts   []Mount
	Memory   int64
	NanoCPUs int64
	Pids     int64
	Network  string
	Runtime  string
	Tmpfs    map[string]string
}

type ContainerInfo struct {
	ID      string
	Name    string
	Running bool
	Labels  map[string]string
}

type ExecSpec struct {
	Container string
	Cmd       []string
	Env       []string
	WorkDir   string
	Tty       bool
	Stdin     bool
}

type Process interface {
	Stdout() io.Reader
	StderrTail() string
	Write(p []byte) (int, error)
	CloseStdin() error
	Wait(ctx context.Context) (int, error)
	Close() error
}

type Engine interface {
	Ping(ctx context.Context) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	EnsureNetwork(ctx context.Context, name string) error
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string) error
	InspectContainer(ctx context.Context, id string) (ContainerInfo, error)
	ListContainers(ctx context.Context, label string) ([]ContainerInfo, error)
	Exec(ctx context.Context, spec ExecSpec) (Process, error)
	ContainerStats(ctx context.Context, id string) (Stats, error)
	VolumeSizes(ctx context.Context) (map[string]int64, error)
}
