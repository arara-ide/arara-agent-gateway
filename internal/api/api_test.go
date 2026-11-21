package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/api"
	"github.com/arara-ide/arara-agent-gateway/internal/connect"
	"github.com/arara-ide/arara-agent-gateway/internal/docker/fakedocker"
	"github.com/arara-ide/arara-agent-gateway/internal/github"
	"github.com/arara-ide/arara-agent-gateway/internal/run"
	"github.com/arara-ide/arara-agent-gateway/internal/testutil"
)

const adminKey = "admin-key-0123456789-0123456789-abcdef"

func handler(e *fakedocker.Exec) int {
	cmd := e.Spec.Cmd
	if cmd[0] == "timeout" && len(cmd) > 2 && cmd[2] == "claude" {
		sc := bufio.NewScanner(e.Stdin)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, `"initialize"`) {
				fmt.Fprintln(e.Stdout, `{"type":"control_response","response":{"subtype":"success","request_id":"arara-init","response":{}}}`)
			}
			if strings.Contains(line, `"get_usage"`) {
				fmt.Fprintln(e.Stdout, `{"type":"control_response","response":{"subtype":"success","request_id":"arara-usage","response":{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":42}}}}}`)
				return 0
			}
		}
		return 0
	}
	prompt, _ := io.ReadAll(e.Stdin)
	switch {
	case cmd[0] == "claude" && cmd[1] == "auth":
		fmt.Fprint(e.Stdout, `{"loggedIn":true,"authMethod":"claude.ai","email":"dev@example.com"}`)
	case cmd[0] == "codex" && cmd[1] == "login":
		fmt.Fprintln(e.Stdout, "Not logged in")
		return 1
	case cmd[0] == "agy" && len(cmd) > 1 && cmd[len(cmd)-1] == "models":
		fmt.Fprintln(e.Stderr, "Error: Please sign in")
		return 1
	case cmd[0] == "claude":
		fmt.Fprintln(e.Stdout, `{"type":"system","subtype":"init","session_id":"sess-1"}`)
		fmt.Fprintf(e.Stdout, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":%q}}}`+"\n", string(prompt))
		fmt.Fprintf(e.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"sess-1"}`+"\n", string(prompt))
	}
	return 0
}

type harness struct {
	srv *httptest.Server
	env *testutil.Env
}

func newHarness(t *testing.T) *harness {
	env := testutil.NewEnv(t, 10)
	env.Engine.SetHandler(handler)
	log := testutil.Logger()
	conn := connect.New(env.Store, env.Box, env.Instances, env.Engine, env.Clock.Now, log)
	gh := github.New(github.Config{}, env.Store, env.Box, nil, env.Clock.Now)
	ws := github.NewWorkspace(gh, env.Engine)
	runs := run.NewManager(env.Store, env.Instances, env.Engine, conn, ws, run.Settings{MaxPerInstance: 3, Timeout: time.Minute}, env.Clock.Now, log)
	web := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>dashboard")}}
	s := api.New(api.Deps{Store: env.Store, Engine: env.Engine, Instances: env.Instances, Runs: runs, Connections: conn, GitHub: gh, Workspace: ws, APIKey: adminKey, IdleTimeout: 5 * time.Minute, Web: web, OpenAPI: []byte("openapi: 3.1.0"), Log: log, Version: "test"})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, env: env}
}

type resp struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

func (h *harness) do(t *testing.T, method, path, key string, body any) resp {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, r)
	if key != "" {
		req.Header.Set("apikey", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, raw: string(raw), header: res.Header}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func errCode(r resp) string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func (h *harness) instance(t *testing.T, name string) string {
	r := h.do(t, "POST", "/api/instances", adminKey, map[string]string{"name": name})
	if r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.raw)
	}
	return r.body["instance"].(map[string]any)["id"].(string)
}

func TestAPIKeyAuthentication(t *testing.T) {
	h := newHarness(t)
	if r := h.do(t, "GET", "/api/instances", "", nil); r.status != 401 || errCode(r) != "unauthorized" {
		t.Fatalf("missing key: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances", "wrong", nil); r.status != 401 {
		t.Fatalf("wrong key: %d", r.status)
	}
	if r := h.do(t, "GET", "/api/instances", adminKey, nil); r.status != 200 {
		t.Fatalf("admin key: %d", r.status)
	}
	if r := h.do(t, "GET", "/api/health", "", nil); r.status != 200 {
		t.Fatalf("health must be public: %d", r.status)
	}
}

func TestCreateMultipleInstancesDoesNotStart(t *testing.T) {
	h := newHarness(t)
	first := h.do(t, "POST", "/api/instances", adminKey, nil)
	second := h.do(t, "POST", "/api/instances", adminKey, map[string]string{"name": "second"})
	if first.status != 201 || second.status != 201 {
		t.Fatalf("statuses %d %d", first.status, second.status)
	}
	a := first.body["instance"].(map[string]any)
	b := second.body["instance"].(map[string]any)
	if a["state"] != "stopped" || a["id"] == b["id"] {
		t.Fatalf("instances %v %v", a, b)
	}
	list := h.do(t, "GET", "/api/instances", adminKey, nil)
	if len(list.body["instances"].([]any)) != 2 {
		t.Fatalf("list %s", list.raw)
	}
}

func TestDeleteRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	id := h.instance(t, "one")
	if r := h.do(t, "DELETE", "/api/instances/"+id, adminKey, nil); r.status != 400 || errCode(r) != "confirmation_required" {
		t.Fatalf("delete without confirm: %d", r.status)
	}
	if r := h.do(t, "DELETE", "/api/instances/"+id+"/github", adminKey, nil); r.status == 200 {
		t.Fatalf("manual github disconnect still exposed: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "DELETE", "/api/instances/"+id+"?confirm="+id, adminKey, nil); r.status != 204 {
		t.Fatalf("delete: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances/"+id, adminKey, nil); r.status != 404 {
		t.Fatalf("after delete: %d", r.status)
	}
}

func TestRunWithStreaming(t *testing.T) {
	h := newHarness(t)
	id := h.instance(t, "one")
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/claude/run", adminKey, map[string]string{"prompt": "hi"}); r.status != 409 || errCode(r) != "instance_not_running" {
		t.Fatalf("run on stopped: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/start", adminKey, nil); r.status != 200 {
		t.Fatalf("start: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/codex/run", adminKey, map[string]string{"prompt": "hi"}); r.status != 409 || errCode(r) != "agent_not_authenticated" {
		t.Fatalf("codex without login: %d %s", r.status, r.raw)
	}
	r := h.do(t, "POST", "/api/instances/"+id+"/agents/claude/run", adminKey, map[string]string{"prompt": "hello stream"})
	if r.status != 202 {
		t.Fatalf("run: %d %s", r.status, r.raw)
	}
	streamURL := r.body["stream_url"].(string)

	req, _ := http.NewRequestWithContext(context.Background(), "GET", h.srv.URL+streamURL, nil)
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("stream without key: %d", res.StatusCode)
	}

	req.Header.Set("apikey", adminKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %s", ct)
	}
	var types []string
	var done map[string]any
	sc := bufio.NewScanner(res.Body)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			event = v
			types = append(types, v)
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok && event == "done" {
			_ = json.Unmarshal([]byte(v), &done)
			break
		}
	}
	if strings.Join(types, ",") != "status,session,delta,done" {
		t.Fatalf("events %v", types)
	}
	if done["status"] != "succeeded" || done["response"] != "hello stream" || done["session_id"] != "sess-1" {
		t.Fatalf("done %v", done)
	}
	hist := h.do(t, "GET", "/api/instances/"+id+"/history", adminKey, nil)
	runs := hist.body["runs"].([]any)
	if len(runs) != 1 || hist.body["limit"].(float64) != 20 {
		t.Fatalf("history %s", hist.raw)
	}
}

func TestSecretsNeverReturnedAndIsolatedPerInstance(t *testing.T) {
	h := newHarness(t)
	a := h.instance(t, "a")
	b := h.instance(t, "b")
	r := h.do(t, "PUT", "/api/instances/"+a+"/agents/antigravity/api-key", adminKey, map[string]string{"api_key": "AIzaSuperSecretValue123"})
	if r.status != 200 || strings.Contains(r.raw, "SuperSecret") {
		t.Fatalf("set key: %d %s", r.status, r.raw)
	}
	connsA := h.do(t, "GET", "/api/instances/"+a+"/connections", adminKey, nil)
	if strings.Contains(connsA.raw, "SuperSecret") || strings.Contains(connsA.raw, "v1:") || !strings.Contains(connsA.raw, `"status":"connected"`) {
		t.Fatalf("connections a %s", connsA.raw)
	}
	connsB := h.do(t, "GET", "/api/instances/"+b+"/connections", adminKey, nil)
	if strings.Contains(connsB.raw, `"status":"connected"`) {
		t.Fatalf("connections b leaked from a: %s", connsB.raw)
	}
	if r := h.do(t, "PUT", "/api/instances/"+a+"/agents/antigravity/api-key", adminKey, map[string]string{"api_key": "a b"}); r.status != 400 {
		t.Fatalf("invalid key accepted: %d", r.status)
	}
	if r := h.do(t, "POST", "/api/instances/x/agents/antigravity/login", adminKey, nil); r.status != 404 {
		t.Fatalf("login on missing instance: %d", r.status)
	}
}

func TestValidationErrors(t *testing.T) {
	h := newHarness(t)
	id := h.instance(t, "one")
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/claude/run", adminKey, map[string]any{"prompt": "x", "extra": 1}); r.status != 400 || errCode(r) != "invalid_json" {
		t.Fatalf("unknown field: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/gemini/run", adminKey, map[string]any{"prompt": "x"}); r.status != 404 || errCode(r) != "unknown_agent" {
		t.Fatalf("unknown agent: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/claude/run", adminKey, map[string]any{"prompt": ""}); r.status != 400 || errCode(r) != "invalid_prompt" {
		t.Fatalf("empty prompt: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/github/connect", adminKey, nil); r.status != 503 || errCode(r) != "github_not_configured" {
		t.Fatalf("github disabled: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/nope", adminKey, nil); r.status != 404 || errCode(r) != "not_found" {
		t.Fatalf("unknown route: %d", r.status)
	}
}

func TestDashboardHeaders(t *testing.T) {
	h := newHarness(t)
	res, err := http.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	csp := res.Header.Get("Content-Security-Policy")
	if res.StatusCode != 200 || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("dashboard %d csp=%s", res.StatusCode, csp)
	}
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), adminKey) {
		t.Fatal("api key embedded in dashboard")
	}
}

func TestAgentUsageIsReadPerInstanceWithoutPrompt(t *testing.T) {
	h := newHarness(t)
	a := h.instance(t, "a")
	b := h.instance(t, "b")
	if r := h.do(t, "GET", "/api/instances/"+a+"/agents/claude/usage", adminKey, nil); r.status != 409 || errCode(r) != "instance_not_running" {
		t.Fatalf("usage on stopped: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+a+"/start", adminKey, nil); r.status != 200 {
		t.Fatalf("start: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances/"+a+"/agents", adminKey, nil); r.status != 200 || !strings.Contains(r.raw, `"connected"`) {
		t.Fatalf("agents: %d %s", r.status, r.raw)
	}
	r := h.do(t, "GET", "/api/instances/"+a+"/agents/claude/usage", adminKey, nil)
	usage, _ := r.body["usage"].(map[string]any)
	if r.status != 200 || usage["rate_limits_available"] != true {
		t.Fatalf("usage: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances/"+a+"/agents/codex/usage", adminKey, nil); r.status != 200 || !strings.Contains(r.raw, `"usage":null`) {
		t.Fatalf("codex without payload: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances/"+b+"/agents/claude/usage", adminKey, nil); r.status != 409 {
		t.Fatalf("usage on other stopped instance: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances/"+a+"/agents/nope/usage", adminKey, nil); r.status != 404 {
		t.Fatalf("unknown agent: %d %s", r.status, r.raw)
	}
	for _, spec := range h.env.Engine.ExecLog() {
		line := strings.Join(spec.Cmd, " ")
		if strings.Contains(line, "app-server") {
			t.Fatalf("usage probe ran for a disconnected agent: %s", line)
		}
		if strings.HasPrefix(line, "timeout ") && !strings.Contains(spec.Container, strings.TrimPrefix(a, "inst_")) {
			t.Fatalf("usage ran outside instance a: %s", spec.Container)
		}
	}
}
