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

const (
	adminKey       = "admin-key-0123456789-0123456789-abcdef"
	integrationKey = "integration-key-0123456789-0123456789-x"
)

func handler(e *fakedocker.Exec) int {
	prompt, _ := io.ReadAll(e.Stdin)
	cmd := e.Spec.Cmd
	switch {
	case cmd[0] == "claude" && cmd[1] == "auth":
		fmt.Fprint(e.Stdout, `{"loggedIn":true,"authMethod":"claude.ai","email":"dev@example.com"}`)
	case cmd[0] == "codex" && cmd[1] == "login":
		fmt.Fprintln(e.Stdout, "Not logged in")
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
	s := api.New(api.Deps{Store: env.Store, Engine: env.Engine, Instances: env.Instances, Runs: runs, Connections: conn, GitHub: gh, Workspace: ws, APIKey: adminKey, IntegrationAPIKey: integrationKey, IdleTimeout: 5 * time.Minute, Web: web, OpenAPI: []byte("openapi: 3.1.0"), Log: log, Version: "test"})
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

func (h *harness) do(t *testing.T, method, path, key, user string, body any) resp {
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
	if user != "" {
		req.Header.Set("X-User-Id", user)
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

func (h *harness) instance(t *testing.T, user string) string {
	r := h.do(t, "POST", "/api/instances", adminKey, "", map[string]string{"external_user_id": user})
	if r.status != http.StatusCreated && r.status != http.StatusOK {
		t.Fatalf("create: %d %s", r.status, r.raw)
	}
	return r.body["instance"].(map[string]any)["id"].(string)
}

func TestAPIKeyAuthentication(t *testing.T) {
	h := newHarness(t)
	if r := h.do(t, "GET", "/api/instances", "", "", nil); r.status != 401 || errCode(r) != "unauthorized" {
		t.Fatalf("missing key: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances", "wrong", "", nil); r.status != 401 {
		t.Fatalf("wrong key: %d", r.status)
	}
	if r := h.do(t, "GET", "/api/instances", adminKey, "", nil); r.status != 200 {
		t.Fatalf("admin key: %d", r.status)
	}
	if r := h.do(t, "GET", "/api/instances", integrationKey, "", nil); r.status != 400 || errCode(r) != "user_scope_required" {
		t.Fatalf("integration without user: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances", integrationKey, "bad user!", nil); r.status != 400 {
		t.Fatalf("invalid user header: %d", r.status)
	}
	if r := h.do(t, "GET", "/api/health", "", "", nil); r.status != 200 {
		t.Fatalf("health must be public: %d", r.status)
	}
}

func TestCrossUserAccessIsDenied(t *testing.T) {
	h := newHarness(t)
	idA := h.instance(t, "user-a")
	h.instance(t, "user-b")
	paths := []struct{ method, path string }{
		{"GET", "/api/instances/" + idA},
		{"GET", "/api/instances/" + idA + "/status"},
		{"GET", "/api/instances/" + idA + "/history"},
		{"POST", "/api/instances/" + idA + "/start"},
		{"POST", "/api/instances/" + idA + "/stop"},
		{"DELETE", "/api/instances/" + idA + "?confirm=" + idA},
		{"GET", "/api/instances/" + idA + "/runs/run_x/stream"},
		{"GET", "/api/users/user-a/instance"},
		{"GET", "/api/users/user-a/connections"},
		{"GET", "/api/users/user-a/github"},
	}
	for _, key := range []string{integrationKey, adminKey} {
		for _, p := range paths {
			r := h.do(t, p.method, p.path, key, "user-b", nil)
			if r.status != 404 {
				t.Fatalf("%s %s with key %s as user-b: %d %s", p.method, p.path, key[:5], r.status, r.raw)
			}
		}
	}
	list := h.do(t, "GET", "/api/instances", integrationKey, "user-b", nil)
	for _, in := range list.body["instances"].([]any) {
		if in.(map[string]any)["external_user_id"] != "user-b" {
			t.Fatal("list leaked another user's instance")
		}
	}
	if r := h.do(t, "POST", "/api/instances", integrationKey, "user-b", map[string]string{"external_user_id": "user-a"}); r.status != 403 || errCode(r) != "user_mismatch" {
		t.Fatalf("create for other user: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "PUT", "/api/users/user-a/agents/claude/api-key", integrationKey, "user-b", map[string]string{"api_key": "sk-ant-123456789"}); r.status != 404 {
		t.Fatalf("set key for other user: %d", r.status)
	}
	if r := h.do(t, "GET", "/api/instances/"+idA, integrationKey, "user-a", nil); r.status != 200 {
		t.Fatalf("owner access: %d", r.status)
	}
}

func TestCreateIsIdempotentAndDoesNotStart(t *testing.T) {
	h := newHarness(t)
	first := h.do(t, "POST", "/api/instances", integrationKey, "user-1", map[string]string{})
	second := h.do(t, "POST", "/api/instances", integrationKey, "user-1", map[string]string{"name": "x"})
	if first.status != 201 || second.status != 200 {
		t.Fatalf("statuses %d %d", first.status, second.status)
	}
	in := first.body["instance"].(map[string]any)
	if in["state"] != "stopped" || in["id"] != second.body["instance"].(map[string]any)["id"] {
		t.Fatalf("instance %v", in)
	}
}

func TestDeleteRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	id := h.instance(t, "user-1")
	if r := h.do(t, "DELETE", "/api/instances/"+id, adminKey, "", nil); r.status != 400 || errCode(r) != "confirmation_required" {
		t.Fatalf("delete without confirm: %d", r.status)
	}
	if r := h.do(t, "DELETE", "/api/instances/"+id+"?confirm="+id, adminKey, "", nil); r.status != 204 {
		t.Fatalf("delete: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/instances/"+id, adminKey, "", nil); r.status != 404 {
		t.Fatalf("after delete: %d", r.status)
	}
}

func TestRunWithStreaming(t *testing.T) {
	h := newHarness(t)
	id := h.instance(t, "user-1")
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/claude/run", integrationKey, "user-1", map[string]string{"prompt": "hi"}); r.status != 409 || errCode(r) != "instance_not_running" {
		t.Fatalf("run on stopped: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/start", integrationKey, "user-1", nil); r.status != 200 {
		t.Fatalf("start: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/codex/run", integrationKey, "user-1", map[string]string{"prompt": "hi"}); r.status != 409 || errCode(r) != "agent_not_authenticated" {
		t.Fatalf("codex without login: %d %s", r.status, r.raw)
	}
	r := h.do(t, "POST", "/api/instances/"+id+"/agents/claude/run", integrationKey, "user-1", map[string]string{"prompt": "hello stream"})
	if r.status != 202 {
		t.Fatalf("run: %d %s", r.status, r.raw)
	}
	streamURL := r.body["stream_url"].(string)

	req, _ := http.NewRequestWithContext(context.Background(), "GET", h.srv.URL+streamURL, nil)
	req.Header.Set("apikey", integrationKey)
	req.Header.Set("X-User-Id", "user-2")
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("stream for other user: %d", res.StatusCode)
	}
	req.Header.Del("apikey")
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("stream without key: %d", res.StatusCode)
	}

	req.Header.Set("apikey", integrationKey)
	req.Header.Set("X-User-Id", "user-1")
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
	hist := h.do(t, "GET", "/api/instances/"+id+"/history", integrationKey, "user-1", nil)
	runs := hist.body["runs"].([]any)
	if len(runs) != 1 || hist.body["limit"].(float64) != 20 {
		t.Fatalf("history %s", hist.raw)
	}
}

func TestSecretsNeverReturned(t *testing.T) {
	h := newHarness(t)
	h.instance(t, "user-1")
	r := h.do(t, "PUT", "/api/users/user-1/agents/antigravity/api-key", integrationKey, "user-1", map[string]string{"api_key": "AIzaSuperSecretValue123"})
	if r.status != 200 || strings.Contains(r.raw, "SuperSecret") {
		t.Fatalf("set key: %d %s", r.status, r.raw)
	}
	conns := h.do(t, "GET", "/api/users/user-1/connections", adminKey, "", nil)
	if strings.Contains(conns.raw, "SuperSecret") || strings.Contains(conns.raw, "v1:") || !strings.Contains(conns.raw, `"status":"connected"`) {
		t.Fatalf("connections %s", conns.raw)
	}
	if r := h.do(t, "PUT", "/api/users/user-1/agents/antigravity/api-key", adminKey, "", map[string]string{"api_key": "a b"}); r.status != 400 {
		t.Fatalf("invalid key accepted: %d", r.status)
	}
	if r := h.do(t, "POST", "/api/instances/x/agents/antigravity/login", adminKey, "", nil); r.status != 404 {
		t.Fatalf("login on missing instance: %d", r.status)
	}
}

func TestValidationErrors(t *testing.T) {
	h := newHarness(t)
	id := h.instance(t, "user-1")
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/claude/run", adminKey, "", map[string]any{"prompt": "x", "extra": 1}); r.status != 400 || errCode(r) != "invalid_json" {
		t.Fatalf("unknown field: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/gemini/run", adminKey, "", map[string]any{"prompt": "x"}); r.status != 404 || errCode(r) != "unknown_agent" {
		t.Fatalf("unknown agent: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/instances/"+id+"/agents/claude/run", adminKey, "", map[string]any{"prompt": ""}); r.status != 400 || errCode(r) != "invalid_prompt" {
		t.Fatalf("empty prompt: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "POST", "/api/users/user-1/github/connect", adminKey, "", nil); r.status != 503 || errCode(r) != "github_not_configured" {
		t.Fatalf("github disabled: %d %s", r.status, r.raw)
	}
	if r := h.do(t, "GET", "/api/nope", adminKey, "", nil); r.status != 404 || errCode(r) != "not_found" {
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
