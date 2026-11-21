package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/arara-ide/arara-agent-gateway/internal/docker/fakedocker"
	"github.com/arara-ide/arara-agent-gateway/internal/github"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
)

func TestRepositoryChangesRoute(t *testing.T) {
	h := newHarness(t)
	first := h.instance(t, "first")
	second := h.instance(t, "second")
	path := func(id string) string { return "/api/instances/" + id + "/repos/changes" }
	query := "?full_name=octo%2Fapp&agent=codex"
	for _, tc := range []struct {
		path, key string
		status    int
	}{
		{path(first) + query, "", http.StatusUnauthorized},
		{path("inst_missing") + query, adminKey, http.StatusNotFound},
		{path(first) + query, adminKey, http.StatusConflict},
	} {
		if r := h.do(t, "GET", tc.path, tc.key, nil); r.status != tc.status {
			t.Fatalf("GET %s: %d %s", tc.path, r.status, r.raw)
		}
	}
	for _, id := range []string{first, second} {
		if r := h.do(t, "POST", "/api/instances/"+id+"/start", adminKey, nil); r.status != http.StatusOK {
			t.Fatalf("start: %d %s", r.status, r.raw)
		}
	}
	h.env.Engine.SetHandler(func(e *fakedocker.Exec) int {
		cmd := e.Spec.Cmd
		if cmd[0] == "test" && (e.Spec.Container != instance.ContainerName(first) || cmd[2] != github.WorktreeDir("octo", "app", "codex")+"/.git") {
			return 1
		}
		switch {
		case slices.Contains(cmd, "rev-parse"), slices.Contains(cmd, "merge-base"):
			fmt.Fprintln(e.Stdout, "head")
		case slices.Contains(cmd, "status"):
			fmt.Fprint(e.Stdout, " M app.txt\x00")
		case slices.Contains(cmd, "diff"):
			fmt.Fprint(e.Stdout, "+first instance")
		}
		return 0
	})
	r := h.do(t, "GET", path(first)+query, adminKey, nil)
	if r.status != 200 || r.header.Get("Cache-Control") != "no-store" || r.body["has_changes"] != true || r.body["can_push"] != false || r.body["diff"] != nil {
		t.Fatalf("status: %d %s", r.status, r.raw)
	}
	r = h.do(t, "GET", path(first)+query+"&include_diff=true", adminKey, nil)
	if r.status != 200 || r.body["diff"] != "+first instance" {
		t.Fatalf("diff: %d %s", r.status, r.raw)
	}
	r = h.do(t, "GET", path(second)+query+"&include_diff=true", adminKey, nil)
	if r.status != 200 || r.body["has_changes"] != false || r.body["diff"] != nil {
		t.Fatalf("second instance: %d %s", r.status, r.raw)
	}
	for _, tc := range []struct {
		query string
		code  string
	}{
		{"?full_name=..%2Fapp&agent=codex", "invalid_repo"},
		{"?full_name=octo%2Fapp&agent=../codex", "unknown_agent"},
		{query + "&include_diff=yes", "invalid_include_diff"},
	} {
		if r := h.do(t, "GET", path(first)+tc.query, adminKey, nil); errCode(r) != tc.code {
			t.Fatalf("GET %s: %d %s", tc.query, r.status, r.raw)
		}
	}
}
