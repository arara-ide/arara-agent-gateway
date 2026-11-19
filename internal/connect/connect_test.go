package connect_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/connect"
	"github.com/arara-ide/arara-agent-gateway/internal/docker/fakedocker"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
	"github.com/arara-ide/arara-agent-gateway/internal/testutil"
)

type loginCLI struct {
	mu       sync.Mutex
	loggedIn bool
	gotCode  string
}

func (c *loginCLI) handler(e *fakedocker.Exec) int {
	cmd := e.Spec.Cmd
	switch {
	case cmd[0] == "claude" && cmd[2] == "status":
		c.mu.Lock()
		defer c.mu.Unlock()
		fmt.Fprintf(e.Stdout, `{"loggedIn":%v,"email":"dev@example.com"}`, c.loggedIn)
	case cmd[0] == "claude" && cmd[2] == "login":
		fmt.Fprint(e.Stdout, "\x1b[2mOpen this URL:\x1b[0m\r\nhttps://claude.com/cai/oauth/authorize?code=true&state=abc\r\nPaste code here if prompted > ")
		line, _ := bufio.NewReader(e.Stdin).ReadString('\r')
		c.mu.Lock()
		c.gotCode = line
		c.loggedIn = line == "secret-code\r"
		c.mu.Unlock()
		return 0
	case cmd[0] == "codex" && cmd[1] == "login" && len(cmd) > 2:
		fmt.Fprint(e.Stdout, "Open https://auth.openai.com/codex/device and enter WXYZ-12345\n")
		<-e.Killed
		return 143
	case cmd[0] == "codex":
		fmt.Fprintln(e.Stdout, "Not logged in")
		return 1
	}
	return 0
}

func setup(t *testing.T) (*connect.Service, *testutil.Env, *loginCLI, store.Instance) {
	env := testutil.NewEnv(t, 10)
	cli := &loginCLI{}
	env.Engine.SetHandler(cli.handler)
	svc := connect.New(env.Store, env.Box, env.Instances, env.Engine, env.Clock.Now, testutil.Logger())
	return svc, env, cli, env.Running(t, "user-1")
}

func waitStatus(t *testing.T, svc *connect.Service, in store.Instance, agent string, want connect.LoginStatus) connect.Login {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l, err := svc.GetLogin(in.ID, agent)
		if err == nil && l.Status == want {
			return l
		}
		time.Sleep(10 * time.Millisecond)
	}
	l, _ := svc.GetLogin(in.ID, agent)
	t.Fatalf("login status %s, want %s", l.Status, want)
	return l
}

func TestClaudeAccountLogin(t *testing.T) {
	svc, env, cli, in := setup(t)
	l, err := svc.StartLogin(context.Background(), in, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != connect.LoginWaiting || l.URL != "https://claude.com/cai/oauth/authorize?code=true&state=abc" || !l.NeedsInput {
		t.Fatalf("login %+v", l)
	}
	if env.Instances.Busy(in.ID) != 1 {
		t.Fatal("login must keep the instance busy")
	}
	if _, err := svc.SendLoginInput(in.ID, "claude", "bad code\nrm -rf"); !errors.Is(err, connect.ErrInvalidInput) {
		t.Fatalf("multi-line input accepted: %v", err)
	}
	if _, err := svc.SendLoginInput(in.ID, "claude", "secret-code"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, in, "claude", connect.LoginSucceeded)
	cli.mu.Lock()
	got := cli.gotCode
	cli.mu.Unlock()
	if got != "secret-code\r" {
		t.Fatalf("cli received %q", got)
	}
	c := svc.Connection(context.Background(), in.UserID, "claude")
	if c.Status != store.ConnConnected || c.Account != "dev@example.com" || c.Method != "account" {
		t.Fatalf("connection %+v", c)
	}
	deadline := time.Now().Add(2 * time.Second)
	for env.Instances.Busy(in.ID) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if env.Instances.Busy(in.ID) != 0 {
		t.Fatal("login did not release the instance")
	}
	if _, err := svc.SendLoginInput(in.ID, "claude", "again"); !errors.Is(err, connect.ErrLoginNoInput) {
		t.Fatal("input accepted after login finished")
	}
}

func TestCodexDeviceLoginCancel(t *testing.T) {
	svc, _, _, in := setup(t)
	l, err := svc.StartLogin(context.Background(), in, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if l.URL != "https://auth.openai.com/codex/device" || l.Code != "WXYZ-12345" || l.NeedsInput {
		t.Fatalf("login %+v", l)
	}
	again, _ := svc.StartLogin(context.Background(), in, "codex")
	if again.ID != l.ID {
		t.Fatal("second start created a parallel login")
	}
	if _, err := svc.CancelLogin(in.ID, "codex"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, in, "codex", connect.LoginCancelled)
}

func TestAntigravityHasNoAccountLogin(t *testing.T) {
	svc, _, _, in := setup(t)
	if _, err := svc.StartLogin(context.Background(), in, "antigravity"); !errors.Is(err, connect.ErrMethodUnsupported) {
		t.Fatalf("got %v", err)
	}
	if _, err := svc.SetAPIKey(context.Background(), in.UserID, "unknown", "key12345678"); !errors.Is(err, connect.ErrUnknownAgent) {
		t.Fatal("unknown agent accepted")
	}
}

func TestLoginRequiresRunningInstance(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	svc := connect.New(env.Store, env.Box, env.Instances, env.Engine, env.Clock.Now, testutil.Logger())
	in, _, _ := env.Instances.Create(context.Background(), "user-1", "")
	if _, err := svc.StartLogin(context.Background(), in, "claude"); err == nil {
		t.Fatal("login on stopped instance")
	}
}
