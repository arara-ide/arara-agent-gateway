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
	mu            sync.Mutex
	loggedIn      bool
	agyLoggedIn   bool
	codexLoggedIn bool
	gotCode       string
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
	case cmd[0] == "codex" && cmd[1] == "login" && len(cmd) > 2 && cmd[2] == "--device-auth":
		fmt.Fprint(e.Stdout, "Open https://auth.openai.com/codex/device and enter WXYZ-12345\n")
		c.mu.Lock()
		loggedIn := c.codexLoggedIn
		c.mu.Unlock()
		if loggedIn {
			return 0
		}
		<-e.Killed
		return 143
	case cmd[0] == "codex":
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.codexLoggedIn {
			fmt.Fprintln(e.Stderr, "Logged in using ChatGPT")
			return 0
		}
		fmt.Fprintln(e.Stderr, "Not logged in")
		return 1
	case cmd[0] == "agy" && len(cmd) > 1 && cmd[len(cmd)-1] == "models":
		c.mu.Lock()
		ok := c.agyLoggedIn
		c.mu.Unlock()
		if ok {
			fmt.Fprintln(e.Stdout, `{"conversation_id":"","status":"SUCCESS","response":"gemini-4-neon-high\tGemini 4 Neon"}`)
			return 0
		}
		fmt.Fprintln(e.Stderr, "Error: Please sign in to view available models.")
		return 1
	case cmd[0] == "agy" && len(cmd) > 1 && cmd[len(cmd)-1] == "/logout":
		c.mu.Lock()
		c.agyLoggedIn = false
		c.mu.Unlock()
		fmt.Fprintln(e.Stdout, "Successfully logged out.")
		<-e.Killed
		return 0
	case cmd[0] == "agy":
		fmt.Fprint(e.Stdout, "Welcome to the Antigravity CLI.\r\nSelect login method:\r\n> 1. Google OAuth\r\n")
		rd := bufio.NewReader(e.Stdin)
		_, _ = rd.ReadString('\r')
		fmt.Fprint(e.Stdout, "Open the URL below in your browser:\r\nhttps://accounts.google.com/o/oauth2/auth?client_id=123&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback&response_type=code&state=s1\r\nAfter authenticating, copy the code displayed in the browser and paste it below:\r\n")
		line, _ := rd.ReadString('\r')
		c.mu.Lock()
		c.gotCode = line
		if line == "google-ok-code\r" {
			c.agyLoggedIn = true
			c.mu.Unlock()
			fmt.Fprintln(e.Stdout, "Successfully signed in!")
			<-e.Killed
			return 0
		}
		c.mu.Unlock()
		fmt.Fprintln(e.Stdout, `Got an error: token exchange failed: oauth2: "invalid_grant"`)
		<-e.Killed
		return 0
	}
	return 0
}

func setup(t *testing.T) (*connect.Service, *testutil.Env, *loginCLI, store.Instance) {
	env := testutil.NewEnv(t, 10)
	cli := &loginCLI{}
	env.Engine.SetHandler(cli.handler)
	svc := connect.New(env.Store, env.Box, env.Instances, env.Engine, env.Clock.Now, testutil.Logger())
	return svc, env, cli, env.Running(t, "inst-1")
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
	t.Fatalf("login status %s, want %s (err=%s)", l.Status, want, l.Error)
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
	if _, err := svc.SetAPIKey(context.Background(), in.ID, "claude", "fake-api-key"); !errors.Is(err, connect.ErrLoginInProgress) {
		t.Fatal("API key operation was allowed during account login")
	}
	if _, err := svc.Logout(context.Background(), in, "claude"); !errors.Is(err, connect.ErrLoginInProgress) {
		t.Fatal("logout was allowed during account login")
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
	c := svc.Connection(context.Background(), in.ID, "claude")
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

func TestAntigravityAccountLoginAndLogout(t *testing.T) {
	svc, env, _, in := setup(t)
	l, err := svc.StartLogin(context.Background(), in, "antigravity")
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != connect.LoginWaiting || !l.NeedsInput || l.URL != "https://accounts.google.com/o/oauth2/auth?client_id=123&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback&response_type=code&state=s1" {
		t.Fatalf("login %+v", l)
	}
	if _, err := svc.SendLoginInput(in.ID, "antigravity", "google-ok-code"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, in, "antigravity", connect.LoginSucceeded)
	c := svc.Connection(context.Background(), in.ID, "antigravity")
	if c.Status != store.ConnConnected || c.Account != "Google" || c.Method != "account" {
		t.Fatalf("connection %+v", c)
	}
	out, err := svc.Logout(context.Background(), in, "antigravity")
	if err != nil || out.Status != store.ConnDisconnected {
		t.Fatalf("logout %+v %v", out, err)
	}
	if env.Instances.Busy(in.ID) != 0 {
		t.Fatal("logout left instance busy")
	}
}

func TestAntigravityLoginFailureAndUnknownAgent(t *testing.T) {
	svc, _, _, in := setup(t)
	if _, err := svc.StartLogin(context.Background(), in, "antigravity"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendLoginInput(in.ID, "antigravity", "wrong-code"); err != nil {
		t.Fatal(err)
	}
	l := waitStatus(t, svc, in, "antigravity", connect.LoginFailed)
	if l.Error != "login_failed" {
		t.Fatalf("error %q", l.Error)
	}
	if _, err := svc.SetAPIKey(context.Background(), in.ID, "unknown", "key12345678"); !errors.Is(err, connect.ErrUnknownAgent) {
		t.Fatal("unknown agent accepted")
	}
}

func TestLoginRequiresRunningInstance(t *testing.T) {
	env := testutil.NewEnv(t, 10)
	svc := connect.New(env.Store, env.Box, env.Instances, env.Engine, env.Clock.Now, testutil.Logger())
	in, _ := env.Instances.Create(context.Background(), "one")
	if _, err := svc.StartLogin(context.Background(), in, "claude"); err == nil {
		t.Fatal("login on stopped instance")
	}
}

func TestCodexAccountLoginReadsStatusFromStderr(t *testing.T) {
	svc, _, cli, in := setup(t)
	cli.mu.Lock()
	cli.codexLoggedIn = true
	cli.mu.Unlock()
	if _, err := svc.StartLogin(context.Background(), in, "codex"); err != nil {
		t.Fatal(err)
	}
	l := waitStatus(t, svc, in, "codex", connect.LoginSucceeded)
	if l.URL != "" || l.Code != "" {
		t.Fatal("completed login retained the device challenge")
	}
	if conn := svc.Connection(context.Background(), in.ID, "codex"); conn.Account != "ChatGPT" {
		t.Fatalf("unexpected Codex account status %s", conn.Status)
	}
}
