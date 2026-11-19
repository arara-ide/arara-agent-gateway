package instance_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
	"github.com/arara-ide/arara-agent-gateway/internal/testutil"
)

func TestCreateDoesNotStartContainer(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	in, created, err := env.Instances.Create(context.Background(), "user-1", "main")
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if in.State != store.StateStopped {
		t.Fatalf("state %s", in.State)
	}
	if _, ok := env.Engine.Container(instance.ContainerName(in.ID)); ok {
		t.Fatal("container created on instance creation")
	}
	again, created, err := env.Instances.Create(context.Background(), "user-1", "other")
	if err != nil || created || again.ID != in.ID {
		t.Fatalf("second create should return existing: %v %v", created, err)
	}
}

func TestStartAppliesIsolationAndIsIdempotent(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	in := env.Running(t, "user-1")
	if in.State != store.StateRunning {
		t.Fatalf("state %s", in.State)
	}
	c, ok := env.Engine.Container(instance.ContainerName(in.ID))
	if !ok || !c.Running {
		t.Fatal("container not running")
	}
	spec := c.Spec
	if spec.User != "1000:1000" || spec.Memory != 1<<30 || spec.Pids != 256 || spec.Network != "aag-test" || spec.Image != testutil.Image {
		t.Fatalf("spec %+v", spec)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Volume != instance.VolumeName(in.ID) || spec.Mounts[0].Target != instance.HomeDir {
		t.Fatalf("mounts %+v", spec.Mounts)
	}
	for _, e := range spec.Env {
		if slices.Contains([]string{"ANTHROPIC_API_KEY", "CODEX_API_KEY", "GEMINI_API_KEY"}, e) {
			t.Fatal("credential in container env")
		}
	}
	again, err := env.Instances.Start(context.Background(), in.ID)
	if err != nil || again.State != store.StateRunning {
		t.Fatalf("idempotent start: %v", err)
	}
}

func TestStopPreservesVolumeAndDeleteRemovesIt(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	ctx := context.Background()
	in := env.Running(t, "user-1")
	out, err := env.Instances.Stop(ctx, in.ID)
	if err != nil || out.State != store.StateStopped {
		t.Fatalf("stop: %v %s", err, out.State)
	}
	if _, ok := env.Engine.Container(instance.ContainerName(in.ID)); ok {
		t.Fatal("container kept after stop")
	}
	if !env.Engine.HasVolume(instance.VolumeName(in.ID)) {
		t.Fatal("volume removed on stop")
	}
	if _, err := env.Instances.Stop(ctx, in.ID); err != nil {
		t.Fatalf("idempotent stop: %v", err)
	}
	if err := env.Instances.Delete(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	if env.Engine.HasVolume(instance.VolumeName(in.ID)) {
		t.Fatal("volume kept after delete")
	}
	if _, err := env.Store.GetInstance(ctx, in.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("instance kept after delete")
	}
}

func TestCapacityLimit(t *testing.T) {
	env := testutil.NewEnv(t, 1)
	env.Running(t, "user-1")
	other, _, _ := env.Instances.Create(context.Background(), "user-2", "")
	if _, err := env.Instances.Start(context.Background(), other.ID); !errors.Is(err, instance.ErrCapacity) {
		t.Fatalf("expected capacity error, got %v", err)
	}
	got, _ := env.Store.GetInstance(context.Background(), other.ID)
	if got.State != store.StateStopped {
		t.Fatalf("rejected instance changed state to %s", got.State)
	}
}

func TestMissingImage(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	delete(env.Engine.Images, testutil.Image)
	in, _, _ := env.Instances.Create(context.Background(), "user-1", "")
	if _, err := env.Instances.Start(context.Background(), in.ID); !errors.Is(err, instance.ErrImageMissing) {
		t.Fatalf("expected image missing, got %v", err)
	}
}

func TestAcquireRequiresRunning(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	in, _, _ := env.Instances.Create(context.Background(), "user-1", "")
	if _, err := env.Instances.Acquire(context.Background(), in.ID); !errors.Is(err, instance.ErrNotRunning) {
		t.Fatalf("expected not running, got %v", err)
	}
}

func TestReconcileAfterRestart(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	ctx := context.Background()
	alive := env.Running(t, "user-1")
	dead := env.Running(t, "user-2")
	_ = env.Engine.RemoveContainer(ctx, instance.ContainerName(dead.ID))
	_, _ = env.Engine.CreateContainer(ctx, dockerOrphan())
	_ = env.Engine.StartContainer(ctx, "aag-inst_orphan")
	_ = env.Store.InsertRun(ctx, store.Run{ID: "run_x", InstanceID: alive.ID, Agent: "claude", Prompt: "p", Status: store.RunRunning, StartedAt: env.Clock.Now()})

	restarted := instance.NewManager(env.Store, env.Engine, testutil.Settings(10), env.Clock.Now, testutil.Logger())
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := env.Store.GetInstance(ctx, alive.ID)
	d, _ := env.Store.GetInstance(ctx, dead.ID)
	if a.State != store.StateRunning || d.State != store.StateStopped {
		t.Fatalf("alive=%s dead=%s", a.State, d.State)
	}
	if _, ok := env.Engine.Container("aag-inst_orphan"); ok {
		t.Fatal("orphan container kept")
	}
	r, _ := env.Store.GetRun(ctx, alive.ID, "run_x")
	if r.Status != store.RunInterrupted {
		t.Fatalf("run %s", r.Status)
	}
	killed := false
	for _, e := range env.Engine.ExecLog() {
		if len(e.Cmd) == 2 && e.Cmd[0] == "aag-kill" && e.Cmd[1] == "all" {
			killed = true
		}
	}
	if !killed {
		t.Fatal("orphan processes were not cleaned")
	}
}
