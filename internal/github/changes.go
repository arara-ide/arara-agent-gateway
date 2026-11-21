package github

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/docker"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const (
	changesTimeout = 10 * time.Second
	statusLimit    = 1 << 20
	diffLimit      = 256 << 10
	untrackedLimit = 200
)

type Changes struct {
	HasChanges bool     `json:"has_changes"`
	CanPush    bool     `json:"can_push"`
	Diff       string   `json:"diff,omitempty"`
	Untracked  []string `json:"untracked,omitempty"`
	Truncated  bool     `json:"truncated,omitempty"`
}

func (w *Workspace) readGit(ctx context.Context, in store.Instance, dir string, limit int64, args ...string) (docker.Result, error) {
	return w.gitOutput(ctx, in, []string{"GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL="}, limit,
		append([]string{"-C", dir, "-c", "core.fsmonitor=false"}, args...)...)
}

func (w *Workspace) readFilters(ctx context.Context, in store.Instance, dir string) ([]string, error) {
	config, err := w.readGit(ctx, in, dir, statusLimit+1, "config", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process|required)$`)
	if errors.Is(err, ErrGitFailed) && config.ExitCode == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(config.Stdout) > statusLimit {
		return nil, ErrGitFailed
	}
	var args []string
	for key := range strings.SplitSeq(strings.TrimSuffix(config.Stdout, "\x00"), "\x00") {
		if key == "" {
			continue
		}
		value := ""
		if strings.HasSuffix(key, ".required") {
			value = "false"
		}
		args = append(args, "-c", key+"="+value)
	}
	return args, nil
}

func (w *Workspace) Changes(ctx context.Context, in store.Instance, fullName, agentName string, includeDiff bool) (Changes, error) {
	owner, name, err := ParseFullName(fullName)
	if err != nil {
		return Changes{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, changesTimeout)
	defer cancel()
	wt := WorktreeDir(owner, name, agentName)
	ok, err := w.exists(ctx, in, wt+"/.git")
	if err != nil || !ok {
		return Changes{}, err
	}
	head, err := w.readGit(ctx, in, wt, 128, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Changes{}, err
	}
	remote, err := w.readGit(ctx, in, wt, 128, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+AgentBranch(agentName))
	published := err == nil
	if !published {
		if !errors.Is(err, ErrGitFailed) || remote.ExitCode != 1 {
			return Changes{}, err
		}
		remote, err = w.readGit(ctx, in, RepoDir(owner, name), 128, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return Changes{}, err
		}
	}
	headRef, remoteRef := strings.TrimSpace(head.Stdout), strings.TrimSpace(remote.Stdout)
	base, err := w.readGit(ctx, in, wt, 128, "merge-base", headRef, remoteRef)
	if err != nil {
		return Changes{}, err
	}
	baseRef := strings.TrimSpace(base.Stdout)
	committed := headRef != baseRef
	filters, err := w.readFilters(ctx, in, wt)
	if err != nil {
		return Changes{}, err
	}
	status, err := w.readGit(ctx, in, wt, statusLimit+1,
		append(filters, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=dirty")...)
	if err != nil {
		return Changes{}, err
	}
	result := Changes{
		HasChanges: committed || status.Stdout != "",
		CanPush:    committed && (!published || baseRef == remoteRef),
	}
	if !includeDiff || !result.HasChanges {
		return result, nil
	}
	diff, err := w.readGit(ctx, in, wt, diffLimit+1, append(filters, "diff", "--no-ext-diff", "--no-textconv", "--no-color",
		"--ignore-submodules=dirty", "--submodule=short", baseRef, "--")...)
	if err != nil {
		return Changes{}, err
	}
	result.Diff = diff.Stdout
	result.Truncated = len(result.Diff) > diffLimit || len(status.Stdout) > statusLimit
	if len(result.Diff) > diffLimit {
		result.Diff = result.Diff[:diffLimit]
	}
	result.Diff = strings.ToValidUTF8(result.Diff, "\uFFFD")
	paths, morePaths := untrackedPaths(status.Stdout)
	result.Untracked = paths
	result.Truncated = result.Truncated || morePaths
	return result, nil
}

func untrackedPaths(status string) ([]string, bool) {
	if len(status) > statusLimit {
		status = status[:statusLimit]
	}
	var paths []string
	for {
		entry, rest, ok := strings.Cut(status, "\x00")
		if !ok {
			return paths, false
		}
		status = rest
		if path, ok := strings.CutPrefix(entry, "?? "); ok {
			if len(paths) == untrackedLimit {
				return paths, true
			}
			paths = append(paths, path)
		}
	}
}
