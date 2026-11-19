package idle_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/idle"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
	"github.com/arara-ide/arara-agent-gateway/internal/testutil"
)

const defaultTimeout = 300 * time.Second

func setup(t *testing.T, timeout time.Duration) (*testutil.Env, *idle.Manager) {
	env := testutil.NewEnv(t, 10)
	return env, idle.New(env.Store, env.Instances, timeout, 15*time.Second, testutil.Logger())
}

func state(t *testing.T, env *testutil.Env, id string) store.InstanceState {
	in, err := env.Store.GetInstance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func TestStopsAfterFiveMinutes(t *testing.T) {
	env, m := setup(t, defaultTimeout)
	in := env.Running(t, "user-1")
	env.Clock.Advance(299 * time.Second)
	if m.Check(context.Background()) != 0 || state(t, env, in.ID) != store.StateRunning {
		t.Fatal("stopped before timeout")
	}
	env.Clock.Advance(time.Second)
	if m.Check(context.Background()) != 1 || state(t, env, in.ID) != store.StateStopped {
		t.Fatal("not stopped after timeout")
	}
	if !env.Engine.HasVolume(instance.VolumeName(in.ID)) {
		t.Fatal("idle stop removed the volume")
	}
}

func TestConfigurableTimeout(t *testing.T) {
	env, m := setup(t, 60*time.Second)
	in := env.Running(t, "user-1")
	env.Clock.Advance(61 * time.Second)
	m.Check(context.Background())
	if state(t, env, in.ID) != store.StateStopped {
		t.Fatal("custom timeout ignored")
	}
}

func TestActivityResetsCounter(t *testing.T) {
	env, m := setup(t, defaultTimeout)
	in := env.Running(t, "user-1")
	env.Clock.Advance(4 * time.Minute)
	if err := env.Instances.Touch(context.Background(), in.ID); err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(4 * time.Minute)
	m.Check(context.Background())
	if state(t, env, in.ID) != store.StateRunning {
		t.Fatal("activity did not reset the counter")
	}
	env.Clock.Advance(time.Minute)
	m.Check(context.Background())
	if state(t, env, in.ID) != store.StateStopped {
		t.Fatal("not stopped after reset timeout")
	}
}

func TestActiveWorkIsNeverInterrupted(t *testing.T) {
	env, m := setup(t, defaultTimeout)
	in := env.Running(t, "user-1")
	release, err := env.Instances.Acquire(context.Background(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(2 * time.Hour)
	m.Check(context.Background())
	if state(t, env, in.ID) != store.StateRunning {
		t.Fatal("instance stopped while a task was running")
	}
	release()
	env.Clock.Advance(299 * time.Second)
	m.Check(context.Background())
	if state(t, env, in.ID) != store.StateRunning {
		t.Fatal("counter must start at the end of the task")
	}
	env.Clock.Advance(time.Second)
	m.Check(context.Background())
	if state(t, env, in.ID) != store.StateStopped {
		t.Fatal("not stopped after task end + timeout")
	}
}

func TestDoesNotStartStoppedInstances(t *testing.T) {
	env, m := setup(t, defaultTimeout)
	in, _, _ := env.Instances.Create(context.Background(), "user-1", "")
	env.Clock.Advance(time.Hour)
	m.Check(context.Background())
	if state(t, env, in.ID) != store.StateStopped {
		t.Fatal("stopped instance changed state")
	}
	if _, ok := env.Engine.Container(instance.ContainerName(in.ID)); ok {
		t.Fatal("idle manager created a container")
	}
}

func TestNoRaceBetweenAcquireAndIdleStop(t *testing.T) {
	for i := 0; i < 30; i++ {
		env, m := setup(t, defaultTimeout)
		in := env.Running(t, "user-1")
		env.Clock.Advance(time.Hour)
		var wg sync.WaitGroup
		var release func()
		var acquireErr error
		wg.Add(2)
		go func() { defer wg.Done(); m.Check(context.Background()) }()
		go func() { defer wg.Done(); release, acquireErr = env.Instances.Acquire(context.Background(), in.ID) }()
		wg.Wait()
		st := state(t, env, in.ID)
		switch {
		case acquireErr == nil:
			if st != store.StateRunning {
				t.Fatalf("iteration %d: task acquired but instance is %s", i, st)
			}
			release()
		case errors.Is(acquireErr, instance.ErrNotRunning):
			if st != store.StateStopped {
				t.Fatalf("iteration %d: acquire refused but instance is %s", i, st)
			}
		default:
			t.Fatal(acquireErr)
		}
	}
}

func TestRunLoopStopsOnContext(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	m := idle.New(env.Store, env.Instances, defaultTimeout, 10*time.Millisecond, testutil.Logger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle loop did not stop")
	}
}
