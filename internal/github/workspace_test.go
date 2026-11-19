package github_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/arara-ide/arara-agent-gateway/internal/docker/fakedocker"
	"github.com/arara-ide/arara-agent-gateway/internal/github"
)

func TestSyncInjectsTokenOnlyInEnv(t *testing.T) {
	gh, f, env := setup(t)
	in := env.Running(t, "one")
	state := authorize(t, gh, f, in.ID, "abc")
	gh.Complete(context.Background(), in.ID, "abc", state)
	env.Engine.SetHandler(func(e *fakedocker.Exec) int {
		if e.Spec.Cmd[0] == "test" {
			return 1
		}
		return 0
	})
	ws := github.NewWorkspace(gh, env.Engine)
	repo, err := ws.Sync(context.Background(), in, "octo/app")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Path != github.RepoDir("octo", "app") {
		t.Fatalf("path %s", repo.Path)
	}
	var clone []string
	for _, e := range env.Engine.ExecLog() {
		if strings.Contains(strings.Join(e.Cmd, " "), "gho_abc") {
			t.Fatal("token passed as argument")
		}
		if len(e.Cmd) > 1 && e.Cmd[1] == "clone" {
			clone = e.Env
		}
		if len(e.Cmd) > 3 && e.Cmd[3] == "config" && slices.ContainsFunc(e.Env, func(s string) bool { return strings.Contains(s, "AUTHORIZATION") }) {
			t.Fatal("token sent to a command that does not need it")
		}
	}
	if !slices.ContainsFunc(clone, func(s string) bool { return strings.HasPrefix(s, "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic ") }) {
		t.Fatalf("clone env %v", clone)
	}
}

func TestPushRules(t *testing.T) {
	gh, f, env := setup(t)
	in := env.Running(t, "one")
	state := authorize(t, gh, f, in.ID, "abc")
	gh.Complete(context.Background(), in.ID, "abc", state)
	ws := github.NewWorkspace(gh, env.Engine)
	if _, err := ws.Push(context.Background(), in, "octo/app", "claude", "main"); !errors.Is(err, github.ErrInvalidBranch) {
		t.Fatalf("push to main allowed: %v", err)
	}
	if _, err := ws.Push(context.Background(), in, "octo/app", "claude", ""); !errors.Is(err, github.ErrPushNotAllowed) {
		t.Fatalf("expected push permission check, got %v", err)
	}
}

func TestPrepareRequiresClone(t *testing.T) {
	gh, _, env := setup(t)
	in := env.Running(t, "user-1")
	env.Engine.SetHandler(func(e *fakedocker.Exec) int {
		if e.Spec.Cmd[0] == "test" {
			return 1
		}
		return 0
	})
	ws := github.NewWorkspace(gh, env.Engine)
	if _, err := ws.Prepare(context.Background(), in, "codex", "octo/app"); !errors.Is(err, github.ErrRepoNotCloned) {
		t.Fatalf("got %v", err)
	}
	env.Engine.SetHandler(func(e *fakedocker.Exec) int {
		if e.Spec.Cmd[0] == "test" && strings.HasSuffix(e.Spec.Cmd[2], "/codex/.git") {
			return 1
		}
		return 0
	})
	wt, err := ws.Prepare(context.Background(), in, "codex", "octo/app")
	if err != nil || wt != github.WorktreeDir("octo", "app", "codex") {
		t.Fatalf("prepare %s %v", wt, err)
	}
	found := false
	for _, e := range env.Engine.ExecLog() {
		if slices.Contains(e.Cmd, "worktree") && slices.Contains(e.Cmd, "add") && slices.Contains(e.Cmd, "arara/codex") {
			found = true
		}
	}
	if !found {
		t.Fatal("worktree not created on agent branch")
	}
}
