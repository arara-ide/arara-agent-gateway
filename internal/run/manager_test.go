package run_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/agent"
	"github.com/arara-ide/arara-agent-gateway/internal/connect"
	"github.com/arara-ide/arara-agent-gateway/internal/docker/fakedocker"
	"github.com/arara-ide/arara-agent-gateway/internal/run"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
	"github.com/arara-ide/arara-agent-gateway/internal/testutil"
)

type fakeCLIs struct {
	mu       sync.Mutex
	loggedIn map[string]bool
	block    bool
	envs     map[string][]string
	started  chan string
}

func (f *fakeCLIs) handler(e *fakedocker.Exec) int {
	prompt, _ := io.ReadAll(e.Stdin)
	cmd := e.Spec.Cmd
	f.mu.Lock()
	logged := f.loggedIn[e.Spec.Container]
	block := f.block
	if e.ProcID != "" {
		f.envs[e.ProcID] = e.Spec.Env
	}
	f.mu.Unlock()
	switch {
	case cmd[0] == "claude" && len(cmd) > 1 && cmd[1] == "auth":
		fmt.Fprintf(e.Stdout, `{"loggedIn":%v,"authMethod":"claude.ai","email":"dev@example.com"}`, logged)
		if !logged {
			return 1
		}
		return 0
	case cmd[0] == "codex" && len(cmd) > 1 && cmd[1] == "login":
		if logged {
			fmt.Fprintln(e.Stdout, "Logged in using ChatGPT")
			return 0
		}
		fmt.Fprintln(e.Stdout, "Not logged in")
		return 1
	}
	if f.started != nil {
		f.started <- e.ProcID
	}
	if block {
		<-e.Killed
		return 143
	}
	switch cmd[0] {
	case "claude":
		if !logged {
			fmt.Fprintln(e.Stdout, `{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","session_id":"s"}`)
			return 1
		}
		fmt.Fprintln(e.Stdout, `{"type":"system","subtype":"init","session_id":"claude-session"}`)
		for _, w := range strings.Fields(string(prompt)) {
			fmt.Fprintf(e.Stdout, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":%q}}}`+"\n", w+" ")
		}
		fmt.Fprintf(e.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"claude-session"}`+"\n", "echo: "+string(prompt))
	case "codex":
		fmt.Fprintln(e.Stdout, `{"type":"thread.started","thread_id":"codex-thread"}`)
		fmt.Fprintf(e.Stdout, `{"type":"item.completed","item":{"type":"agent_message","text":%q}}`+"\n", "codex: "+string(prompt))
		fmt.Fprintln(e.Stdout, `{"type":"turn.completed"}`)
	case "agy":
		fmt.Fprintln(e.Stdout, `{"event":"init","conversation_id":"agy-conv"}`)
		fmt.Fprintln(e.Stdout, `{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"hi"}}`)
		fmt.Fprintln(e.Stdout, `{"event":"result","result":{"conversation_id":"agy-conv","status":"SUCCESS","response":"hi"}}`)
	}
	return 0
}

type fixture struct {
	env  *testutil.Env
	clis *fakeCLIs
	conn *connect.Service
	runs *run.Manager
}

func newFixture(t *testing.T, maxPerInstance int) *fixture {
	env := testutil.NewEnv(t, 10)
	clis := &fakeCLIs{loggedIn: map[string]bool{}, envs: map[string][]string{}}
	env.Engine.SetHandler(clis.handler)
	conn := connect.New(env.Store, env.Box, env.Instances, env.Engine, env.Clock.Now, testutil.Logger())
	runs := run.NewManager(env.Store, env.Instances, env.Engine, conn, nil, run.Settings{MaxPerInstance: maxPerInstance, Timeout: time.Minute}, env.Clock.Now, testutil.Logger())
	env.Instances.BeforeStop = func(ctx context.Context, id string) { runs.CancelInstance(id, "instance_stopped") }
	return &fixture{env: env, clis: clis, conn: conn, runs: runs}
}

func (f *fixture) login(in store.Instance) {
	f.clis.mu.Lock()
	f.clis.loggedIn["aag-"+in.ID] = true
	f.clis.mu.Unlock()
}

func collect(t *testing.T, f *fixture, in store.Instance, id string) ([]run.StreamEvent, store.Run) {
	t.Helper()
	hub := f.runs.Hub(in.ID, id)
	if hub == nil {
		t.Fatal("no hub for run")
	}
	replay, ch, cancel := hub.Subscribe(0)
	defer cancel()
	events := append([]run.StreamEvent(nil), replay...)
	timeout := time.After(5 * time.Second)
	for !slices.ContainsFunc(events, func(e run.StreamEvent) bool { return e.Type == "done" }) {
		select {
		case ev, ok := <-ch:
			if !ok {
				goto done
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatal("run did not finish")
		}
	}
done:
	r, err := f.env.Store.GetRun(context.Background(), in.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return events, r
}

func TestSingleRunStreamsAndPersists(t *testing.T) {
	f := newFixture(t, 3)
	in := f.env.Running(t, "user-1")
	f.login(in)
	r, err := f.runs.Start(context.Background(), in, "claude", run.Request{Prompt: "hello world"})
	if err != nil {
		t.Fatal(err)
	}
	events, final := collect(t, f, in, r.ID)
	var text strings.Builder
	for _, e := range events {
		if e.Type == "delta" {
			text.WriteString(e.Data.(agent.Event).Text)
		}
	}
	if final.Status != store.RunSucceeded || final.Response != "echo: hello world" || final.SessionID != "claude-session" {
		t.Fatalf("final %+v", final)
	}
	if text.String() != "hello world " {
		t.Fatalf("streamed %q", text.String())
	}
	if f.runs.ActiveCount(in.ID) != 0 || f.env.Instances.Busy(in.ID) != 0 {
		t.Fatal("slot not released")
	}
}

func TestParallelAgentsAndLimits(t *testing.T) {
	f := newFixture(t, 3)
	in := f.env.Running(t, "user-1")
	f.login(in)
	if _, err := f.conn.SetAPIKey(context.Background(), in.UserID, "antigravity", "AIzaTestKey123456"); err != nil {
		t.Fatal(err)
	}
	f.clis.block = true
	f.clis.started = make(chan string, 3)
	ids := map[string]string{}
	for _, a := range []string{"claude", "codex", "antigravity"} {
		r, err := f.runs.Start(context.Background(), in, a, run.Request{Prompt: "work"})
		if err != nil {
			t.Fatalf("%s: %v", a, err)
		}
		ids[a] = r.ID
	}
	for range 3 {
		<-f.clis.started
	}
	if n := f.runs.ActiveCount(in.ID); n != 3 {
		t.Fatalf("active %d", n)
	}
	if _, err := f.runs.Start(context.Background(), in, "claude", run.Request{Prompt: "again"}); !errors.Is(err, run.ErrAgentBusy) {
		t.Fatalf("expected agent busy, got %v", err)
	}
	for a, id := range ids {
		if err := f.runs.Cancel(in.ID, id, "cancelled_by_request"); err != nil {
			t.Fatal(a, err)
		}
		_, final := collect(t, f, in, id)
		if final.Status != store.RunCancelled || final.ErrorCode != "cancelled_by_request" {
			t.Fatalf("%s: %+v", a, final)
		}
	}
}

func TestInstanceAgentLimit(t *testing.T) {
	f := newFixture(t, 1)
	in := f.env.Running(t, "user-1")
	f.login(in)
	f.clis.block = true
	f.clis.started = make(chan string, 2)
	r, err := f.runs.Start(context.Background(), in, "claude", run.Request{Prompt: "a"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.clis.started
	if _, err := f.runs.Start(context.Background(), in, "codex", run.Request{Prompt: "b"}); !errors.Is(err, run.ErrInstanceBusy) {
		t.Fatalf("expected instance busy, got %v", err)
	}
	_ = f.runs.Cancel(in.ID, r.ID, "cancelled_by_request")
	collect(t, f, in, r.ID)
}

func TestNotAuthenticatedIsRejectedBeforeRun(t *testing.T) {
	f := newFixture(t, 3)
	in := f.env.Running(t, "user-1")
	for _, a := range []string{"claude", "codex", "antigravity"} {
		if _, err := f.runs.Start(context.Background(), in, a, run.Request{Prompt: "x"}); !errors.Is(err, run.ErrNotAuthenticated) {
			t.Fatalf("%s: expected not authenticated, got %v", a, err)
		}
	}
	runs, _ := f.env.Store.ListRuns(context.Background(), in.ID)
	if len(runs) != 0 || f.env.Instances.Busy(in.ID) != 0 {
		t.Fatal("rejected runs left state behind")
	}
}

func TestAuthFailureDuringRunMarksConnection(t *testing.T) {
	f := newFixture(t, 3)
	in := f.env.Running(t, "user-1")
	if _, err := f.conn.SetAPIKey(context.Background(), in.UserID, "claude", "sk-ant-invalid-key"); err != nil {
		t.Fatal(err)
	}
	r, err := f.runs.Start(context.Background(), in, "claude", run.Request{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, final := collect(t, f, in, r.ID)
	if final.Status != store.RunFailed || final.ErrorCode != "agent_not_authenticated" {
		t.Fatalf("final %+v", final)
	}
	c := f.conn.Connection(context.Background(), in.UserID, "claude")
	if c.Status != store.ConnError || c.Method != "api_key" {
		t.Fatalf("connection %s %s", c.Status, c.Method)
	}
}

func TestCredentialsAreIsolatedPerUser(t *testing.T) {
	f := newFixture(t, 3)
	a := f.env.Running(t, "user-a")
	b := f.env.Running(t, "user-b")
	ctx := context.Background()
	if _, err := f.conn.SetAPIKey(ctx, "user-a", "antigravity", "AIza-user-a-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.SetAPIKey(ctx, "user-b", "antigravity", "AIza-user-b-secret"); err != nil {
		t.Fatal(err)
	}
	ra, _ := f.runs.Start(ctx, a, "antigravity", run.Request{Prompt: "x"})
	rb, _ := f.runs.Start(ctx, b, "antigravity", run.Request{Prompt: "y"})
	collect(t, f, a, ra.ID)
	collect(t, f, b, rb.ID)
	f.clis.mu.Lock()
	envA, envB := strings.Join(f.clis.envs[ra.ID], ","), strings.Join(f.clis.envs[rb.ID], ",")
	f.clis.mu.Unlock()
	if !strings.Contains(envA, "user-a-secret") || strings.Contains(envA, "user-b") || !strings.Contains(envB, "user-b-secret") || strings.Contains(envB, "user-a") {
		t.Fatalf("credential leak: A=%s B=%s", envA, envB)
	}
	for _, e := range f.env.Engine.ExecLog() {
		if e.Container == "aag-"+a.ID && strings.Contains(strings.Join(e.Env, ","), "user-b") {
			t.Fatal("user-b secret reached user-a container")
		}
	}
	conns, _ := f.env.Store.ListConnections(ctx, "user-a")
	for _, c := range conns {
		if strings.Contains(c.Secret, "user-a-secret") {
			t.Fatal("api key stored in plaintext")
		}
	}
}

func TestStopCancelsActiveRuns(t *testing.T) {
	f := newFixture(t, 3)
	in := f.env.Running(t, "user-1")
	f.login(in)
	f.clis.block = true
	f.clis.started = make(chan string, 1)
	r, _ := f.runs.Start(context.Background(), in, "codex", run.Request{Prompt: "x"})
	<-f.clis.started
	if _, err := f.env.Instances.Stop(context.Background(), in.ID); err != nil {
		t.Fatal(err)
	}
	final, _ := f.env.Store.GetRun(context.Background(), in.ID, r.ID)
	if final.Status != store.RunCancelled || final.ErrorCode != "instance_stopped" {
		t.Fatalf("final %+v", final)
	}
}

func TestValidation(t *testing.T) {
	cases := []run.Request{{Prompt: ""}, {Prompt: strings.Repeat("a", run.MaxPromptBytes+1)}, {Prompt: "x", SessionID: "../etc"}, {Prompt: "x", Model: "--evil"}, {Prompt: "\xff"}}
	for _, c := range cases {
		if run.Validate(c) == nil {
			t.Fatalf("accepted invalid request %+v", c.SessionID)
		}
	}
}

func TestRunOnStoppedInstance(t *testing.T) {
	f := newFixture(t, 3)
	in, _, _ := f.env.Instances.Create(context.Background(), "user-1", "")
	if _, err := f.runs.Start(context.Background(), in, "claude", run.Request{Prompt: "x"}); err == nil {
		t.Fatal("run started on stopped instance")
	}
}
