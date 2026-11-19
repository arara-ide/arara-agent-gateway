package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func replay(t *testing.T, a Adapter, fixture string) ([]Event, Outcome) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := a.NewParser()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		events = append(events, p.Line(sc.Bytes())...)
	}
	return events, p.Outcome()
}

func deltas(events []Event) string {
	var b strings.Builder
	for _, e := range events {
		if e.Type == "delta" {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func TestClaudeTextTurn(t *testing.T) {
	events, out := replay(t, Claude{}, "claude-text.ndjson")
	if events[0].Type != "session" || events[0].SessionID != "8c3389b9-a02c-49e5-94e3-4f7d9912cde9" {
		t.Fatalf("first event %+v", events[0])
	}
	if deltas(events) != "PONG" || out.Response != "PONG" || out.Failed || !out.Completed {
		t.Fatalf("outcome %+v deltas %q", out, deltas(events))
	}
}

func TestClaudeNotLoggedIn(t *testing.T) {
	_, out := replay(t, Claude{}, "claude-noauth.ndjson")
	if !out.Failed || !out.AuthError {
		t.Fatalf("expected auth failure, got %+v", out)
	}
}

func TestClaudeToolUse(t *testing.T) {
	p := Claude{}.NewParser()
	ev := p.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`))
	if len(ev) != 1 || ev[0].Type != "tool" || ev[0].Tool != "Bash" || !strings.Contains(ev[0].Detail, "ls") {
		t.Fatalf("got %+v", ev)
	}
}

func TestCodexTextTurn(t *testing.T) {
	events, out := replay(t, Codex{}, "codex-text.ndjson")
	if events[0].SessionID != "01a1f599-0fe0-70c0-a136-4053b39b17fa" || deltas(events) != "PONG" {
		t.Fatalf("events %+v", events)
	}
	if out.Response != "PONG" || out.Failed || !out.Completed {
		t.Fatalf("outcome %+v", out)
	}
}

func TestCodexUnauthorized(t *testing.T) {
	_, out := replay(t, Codex{}, "codex-noauth.ndjson")
	if !out.Failed || !out.AuthError || !out.Completed {
		t.Fatalf("expected auth failure, got %+v", out)
	}
}

func TestCodexTools(t *testing.T) {
	p := Codex{}.NewParser()
	a := p.Line([]byte(`{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}`))
	b := p.Line([]byte(`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"a.go"},{"path":"b.go"}]}}`))
	if a[0].Tool != "command" || a[0].Detail != "go test ./..." || b[0].Detail != "a.go, b.go" {
		t.Fatalf("got %+v %+v", a, b)
	}
}

func TestAntigravityTextTurn(t *testing.T) {
	events, out := replay(t, Antigravity{}, "antigravity-text.ndjson")
	if events[0].SessionID != "26b2aac9-79e5-4f28-b88a-b0138a5d06a3" || deltas(events) != "PONG\n" {
		t.Fatalf("events %+v", events)
	}
	if out.Response != "PONG\n" || out.Failed || !out.Completed {
		t.Fatalf("outcome %+v", out)
	}
}

func TestAntigravityFailureAndTools(t *testing.T) {
	p := Antigravity{}.NewParser()
	tools := p.Line([]byte(`{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE","step_type":"run_command"}}`))
	again := p.Line([]byte(`{"event":"step_update","step_update":{"step_index":2,"state":"DONE","step_type":"run_command"}}`))
	again = append(again, p.Line([]byte(`{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"error_message"}}`))...)
	p.Line([]byte(`{"event":"result","result":{"conversation_id":"","status":"ERROR","response":"","error":"API key not valid"}}`))
	out := p.Outcome()
	if len(tools) != 1 || len(again) != 0 || !out.Failed || !out.AuthError {
		t.Fatalf("tools=%v again=%v out=%+v", tools, again, out)
	}
}

func TestPromptNeverBecomesFlag(t *testing.T) {
	prompt := "--dangerous flag-like prompt"
	c := Claude{}.Run(RunInput{Prompt: prompt})
	if slices.Contains(c.Args, prompt) || string(c.Stdin) != prompt {
		t.Fatalf("claude must read prompt from stdin: %v", c.Args)
	}
	x := Codex{}.Run(RunInput{Prompt: prompt, SessionID: "abc"})
	if slices.Contains(x.Args, prompt) || x.Args[len(x.Args)-1] != "-" || x.Args[2] != "resume" {
		t.Fatalf("codex args %v", x.Args)
	}
	g := Antigravity{}.Run(RunInput{Prompt: prompt, UseAPIKey: true, SessionID: "conv-1"})
	if !slices.Contains(g.Args, "--print="+prompt) || !slices.Contains(g.Args, `{"modelProvider":"gemini"}`) || !slices.Contains(g.Args, "conv-1") {
		t.Fatalf("antigravity args %v", g.Args)
	}
}

func TestResumeFlags(t *testing.T) {
	c := Claude{}.Run(RunInput{Prompt: "x", SessionID: "s1", Model: "haiku"})
	if !slices.Contains(c.Args, "--resume") || !slices.Contains(c.Args, "s1") || !slices.Contains(c.Args, "haiku") {
		t.Fatalf("claude %v", c.Args)
	}
}

func TestStatusParsing(t *testing.T) {
	if ok, acc := (Claude{}).ParseStatus(0, `{"loggedIn":true,"authMethod":"claude.ai","email":"dev@example.com"}`); !ok || acc != "dev@example.com" {
		t.Fatalf("claude status %v %s", ok, acc)
	}
	if ok, _ := (Claude{}).ParseStatus(1, `{"loggedIn":false}`); ok {
		t.Fatal("claude logged out parsed as logged in")
	}
	if ok, acc := (Codex{}).ParseStatus(0, "Logged in using ChatGPT\n"); !ok || acc != "ChatGPT" {
		t.Fatalf("codex status %v %s", ok, acc)
	}
	if ok, _ := (Codex{}).ParseStatus(1, "Not logged in\n"); ok {
		t.Fatal("codex logged out parsed as logged in")
	}
}

func TestLoginOutputExtraction(t *testing.T) {
	claudeOut := "\x1b[2mBrowser didn't open? Use the url below to sign in (c to copy)\x1b[0m\r\n\r\nhttps://claude.com/cai/oauth/authorize?code=true&client_id=abc&state=xyz\r\n\r\nPaste code here if prompted >"
	if u := FindURL(claudeOut, "oauth", "authorize"); u != "https://claude.com/cai/oauth/authorize?code=true&client_id=abc&state=xyz" {
		t.Fatalf("claude url %q", u)
	}
	codexOut := "Follow these steps to sign in with ChatGPT using device code authorization:\n\n1. Open this link in your browser and sign in to your account\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\n\n2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n   \x1b[94mABCD-12EFG\x1b[0m\n"
	if u := FindURL(codexOut, "auth.openai.com"); u != "https://auth.openai.com/codex/device" {
		t.Fatalf("codex url %q", u)
	}
	if c := FindDeviceCode(codexOut); c != "ABCD-12EFG" {
		t.Fatalf("codex code %q", c)
	}
}

func TestValidation(t *testing.T) {
	for _, ok := range []string{"8c3389b9-a02c-49e5-94e3-4f7d9912cde9", "abc_1"} {
		if !ValidSessionID(ok) {
			t.Fatalf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a b", "../x", strings.Repeat("a", 200)} {
		if ValidSessionID(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
	if !ValidModel("claude-haiku-5-5") || !ValidModel("gpt-5.4-codex") || ValidModel("--model") {
		t.Fatal("model validation")
	}
	if len(Names()) != 3 {
		t.Fatalf("agents %v", Names())
	}
}
