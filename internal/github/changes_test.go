package github_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/arara-ide/arara-agent-gateway/internal/docker/fakedocker"
	"github.com/arara-ide/arara-agent-gateway/internal/github"
)

func TestWorkspaceChanges(t *testing.T) {
	for _, tc := range []struct {
		name, remote, base, status, diff string
		details, absent, changes, push   bool
	}{
		{name: "before first run", absent: true},
		{name: "clean", remote: "head", base: "head", details: true},
		{name: "untracked", remote: "head", base: "head", status: "?? new\nfile\x00", details: true, changes: true},
		{name: "staged", remote: "head", base: "head", status: "M  file\x00", diff: "+new staged line", details: true, changes: true},
		{name: "unpublished commits", base: "base", changes: true, push: true},
		{name: "ahead", remote: "base", base: "base", diff: "+new line", details: true, changes: true, push: true},
		{name: "diverged", remote: "other", base: "base", changes: true},
		{name: "behind", remote: "other", base: "head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, _, env := setup(t)
			in := env.Running(t, "one")
			env.Engine.SetHandler(func(e *fakedocker.Exec) int {
				cmd := e.Spec.Cmd
				switch {
				case cmd[0] == "test":
					if tc.absent {
						return 1
					}
				case slices.Contains(cmd, "rev-parse"):
					switch {
					case cmd[2] == github.RepoDir("octo", "app"):
						fmt.Fprintln(e.Stdout, "base")
					case slices.Contains(cmd, "--quiet"):
						if tc.remote == "" {
							return 1
						}
						fmt.Fprintln(e.Stdout, tc.remote)
					default:
						fmt.Fprintln(e.Stdout, "head")
					}
				case slices.Contains(cmd, "merge-base"):
					fmt.Fprintln(e.Stdout, tc.base)
				case slices.Contains(cmd, "status"):
					fmt.Fprint(e.Stdout, tc.status)
				case slices.Contains(cmd, "diff"):
					fmt.Fprint(e.Stdout, tc.diff)
				}
				return 0
			})
			ws := github.NewWorkspace(gh, env.Engine)
			got, err := ws.Changes(context.Background(), in, "octo/app", "codex", tc.details)
			if err != nil || got.HasChanges != tc.changes || got.CanPush != tc.push || got.Diff != tc.diff || got.Truncated {
				t.Fatalf("changes: %+v, %v", got, err)
			}
			if tc.name == "untracked" && !slices.Equal(got.Untracked, []string{"new\nfile"}) {
				t.Fatalf("untracked: %q", got.Untracked)
			}
			for _, exec := range env.Engine.ExecLog() {
				if !tc.details && slices.Contains(exec.Cmd, "diff") {
					t.Fatal("status request generated a diff")
				}
				if exec.Cmd[0] == "git" && (!slices.Contains(exec.Env, "GIT_OPTIONAL_LOCKS=0") || !slices.Contains(exec.Env, "GIT_ALLOW_PROTOCOL=")) {
					t.Fatal("git ran without read-only options")
				}
			}
		})
	}
}

func TestWorkspaceChangesLimitsAndFailures(t *testing.T) {
	gh, _, env := setup(t)
	in := env.Running(t, "one")
	ws := github.NewWorkspace(gh, env.Engine)
	env.Engine.SetHandler(func(e *fakedocker.Exec) int {
		switch {
		case slices.Contains(e.Spec.Cmd, "rev-parse"), slices.Contains(e.Spec.Cmd, "merge-base"):
			fmt.Fprintln(e.Stdout, "head")
		case slices.Contains(e.Spec.Cmd, "status"):
			for i := range 201 {
				fmt.Fprintf(e.Stdout, "?? file-%d\x00", i)
			}
		case slices.Contains(e.Spec.Cmd, "diff"):
			fmt.Fprint(e.Stdout, strings.Repeat("a", 256<<10)+"more")
		}
		return 0
	})
	got, err := ws.Changes(context.Background(), in, "octo/app", "codex", true)
	if err != nil || !got.Truncated || len(got.Diff) != 256<<10 || len(got.Untracked) != 200 {
		t.Fatalf("diff length %d, untracked count %d, truncated %t, error %v", len(got.Diff), len(got.Untracked), got.Truncated, err)
	}
	env.Engine.SetHandler(func(e *fakedocker.Exec) int {
		if slices.Contains(e.Spec.Cmd, "merge-base") {
			return 128
		}
		return 0
	})
	if _, err := ws.Changes(context.Background(), in, "octo/app", "codex", false); !errors.Is(err, github.ErrGitFailed) {
		t.Fatalf("Git failure: %v", err)
	}
}
