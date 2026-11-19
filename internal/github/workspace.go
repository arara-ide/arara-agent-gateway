package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/docker"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const (
	ReposDir     = instance.WorkDir + "/repos"
	WorktreesDir = instance.WorkDir + "/worktrees"
	BranchPrefix = "arara/"
	gitTimeout   = 10 * time.Minute
)

var (
	ErrRepoNotCloned  = errors.New("repository is not cloned in this instance")
	ErrInvalidBranch  = errors.New("branch must start with arara/ and contain only safe characters")
	ErrGitFailed      = errors.New("git command failed")
	ErrPushNotAllowed = errors.New("the github connection has no push permission on this repository")
	branchPattern     = regexp.MustCompile(`^arara/[A-Za-z0-9._-]{1,60}(/[A-Za-z0-9._-]{1,60}){0,3}$`)
)

type Workspace struct {
	gh     *Service
	engine docker.Engine
}

func NewWorkspace(gh *Service, e docker.Engine) *Workspace {
	return &Workspace{gh: gh, engine: e}
}

func RepoDir(owner, name string) string { return ReposDir + "/" + owner + "/" + name }

func WorktreeDir(owner, name, agentName string) string {
	return WorktreesDir + "/" + owner + "/" + name + "/" + agentName
}

func AgentBranch(agentName string) string { return BranchPrefix + agentName }

func authEnv(token string) []string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + basic,
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=",
	}
}

func (w *Workspace) git(ctx context.Context, in store.Instance, env []string, args ...string) (docker.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	res, err := docker.Output(ctx, w.engine, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: append([]string{"git"}, args...), Env: append([]string{"GIT_TERMINAL_PROMPT=0"}, env...), WorkDir: instance.HomeDir}, nil, 1<<20)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%w: git %s exited %d", ErrGitFailed, args[0], res.ExitCode)
	}
	return res, nil
}

func (w *Workspace) exists(ctx context.Context, in store.Instance, path string) (bool, error) {
	res, err := docker.Output(ctx, w.engine, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: []string{"test", "-e", path}}, nil, 1024)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

type LocalRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Path          string `json:"path"`
}

func (w *Workspace) Sync(ctx context.Context, in store.Instance, fullName string) (LocalRepo, error) {
	repo, tok, err := w.gh.Repo(ctx, in.UserID, fullName)
	if err != nil {
		return LocalRepo{}, err
	}
	owner, name, _ := ParseFullName(repo.FullName)
	dir := RepoDir(owner, name)
	env := authEnv(tok.AccessToken)
	ok, err := w.exists(ctx, in, dir+"/.git")
	if err != nil {
		return LocalRepo{}, err
	}
	if !ok {
		if _, err := docker.Output(ctx, w.engine, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: []string{"mkdir", "-p", ReposDir + "/" + owner}}, nil, 1024); err != nil {
			return LocalRepo{}, err
		}
		if _, err := w.git(ctx, in, env, "clone", "--", "https://github.com/"+owner+"/"+name+".git", dir); err != nil {
			return LocalRepo{}, err
		}
		email := tok.Login + "@users.noreply.github.com"
		if tok.UserID > 0 {
			email = strconv.FormatInt(tok.UserID, 10) + "+" + tok.Login + "@users.noreply.github.com"
		}
		if _, err := w.git(ctx, in, nil, "-C", dir, "config", "user.name", tok.Login); err != nil {
			return LocalRepo{}, err
		}
		if _, err := w.git(ctx, in, nil, "-C", dir, "config", "user.email", email); err != nil {
			return LocalRepo{}, err
		}
	} else {
		if _, err := w.git(ctx, in, env, "-C", dir, "fetch", "--prune", "origin"); err != nil {
			return LocalRepo{}, err
		}
		if _, err := w.git(ctx, in, nil, "-C", dir, "merge", "--ff-only", "@{upstream}"); err != nil {
			return LocalRepo{}, err
		}
	}
	return LocalRepo{FullName: repo.FullName, DefaultBranch: repo.DefaultBranch, Path: dir}, nil
}

func (w *Workspace) List(ctx context.Context, in store.Instance) ([]LocalRepo, error) {
	res, err := docker.Output(ctx, w.engine, docker.ExecSpec{Container: instance.ContainerName(in.ID), Cmd: []string{"find", ReposDir, "-mindepth", "3", "-maxdepth", "3", "-name", ".git", "-type", "d"}}, nil, 1<<20)
	if err != nil {
		return nil, err
	}
	out := []LocalRepo{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		rel, ok := strings.CutPrefix(strings.TrimSuffix(line, "/.git"), ReposDir+"/")
		if !ok {
			continue
		}
		if _, _, err := ParseFullName(rel); err != nil {
			continue
		}
		out = append(out, LocalRepo{FullName: rel, Path: ReposDir + "/" + rel})
	}
	return out, nil
}

func (w *Workspace) Prepare(ctx context.Context, in store.Instance, agentName, fullName string) (string, error) {
	owner, name, err := ParseFullName(fullName)
	if err != nil {
		return "", err
	}
	dir := RepoDir(owner, name)
	ok, err := w.exists(ctx, in, dir+"/.git")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrRepoNotCloned
	}
	wt := WorktreeDir(owner, name, agentName)
	ok, err = w.exists(ctx, in, wt+"/.git")
	if err != nil {
		return "", err
	}
	if ok {
		return wt, nil
	}
	if _, err := w.git(ctx, in, nil, "-C", dir, "worktree", "prune"); err != nil {
		return "", err
	}
	if _, err := w.git(ctx, in, nil, "-C", dir, "worktree", "add", "-B", AgentBranch(agentName), wt, "HEAD"); err != nil {
		return "", err
	}
	return wt, nil
}

func (w *Workspace) Push(ctx context.Context, in store.Instance, fullName, agentName, branch string) (string, error) {
	if branch == "" {
		branch = AgentBranch(agentName)
	}
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") {
		return "", ErrInvalidBranch
	}
	repo, tok, err := w.gh.Repo(ctx, in.UserID, fullName)
	if err != nil {
		return "", err
	}
	if !repo.CanPush {
		return "", ErrPushNotAllowed
	}
	owner, name, _ := ParseFullName(repo.FullName)
	wt := WorktreeDir(owner, name, agentName)
	ok, err := w.exists(ctx, in, wt+"/.git")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrRepoNotCloned
	}
	if _, err := w.git(ctx, in, authEnv(tok.AccessToken), "-C", wt, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return "", err
	}
	return branch, nil
}
