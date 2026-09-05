// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.dev/modules/process"
)

type Repo struct {
	Dir string
	Ctx context.Context
}

type ContextError struct {
	Operation string
	Err       error
}

func (e ContextError) Error() string { return e.Err.Error() }
func (e ContextError) Unwrap() error { return e.Err }

func (r Repo) context() context.Context {
	if r.Ctx != nil {
		return r.Ctx
	}
	return context.Background()
}

func (r Repo) Run(env []string, args ...string) (string, error) {
	return r.RunContext(r.context(), env, args...)
}

func (r Repo) RunContext(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := process.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			operation := "command"
			if len(args) != 0 {
				operation = args[0]
			}
			return strings.TrimSpace(stdout.String()), ContextError{Operation: operation, Err: ctxErr}
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return strings.TrimSpace(stdout.String()), errors.New(message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (r Repo) GitPath(name string) (string, error) {
	path, err := r.Run(nil, "rev-parse", "--git-path", name)
	if err == nil && !filepath.IsAbs(path) {
		path = filepath.Join(r.Dir, path)
	}
	return path, err
}

func (r Repo) Head(ref string) (string, error) {
	return r.Run(nil, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

func (r Repo) CurrentBranch() (string, error) {
	return r.Run(nil, "symbolic-ref", "--quiet", "--short", "HEAD")
}

func (r Repo) ValidateBranch(branch string) error {
	_, err := r.Run(nil, "check-ref-format", "--branch", branch)
	return err
}

func (r Repo) RequireClean() error {
	bare, err := r.Run(nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return err
	}
	if bare == "true" {
		return errors.New("bare repositories are not supported")
	}
	gitDir, err := r.Run(nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		path, err := r.GitPath(marker)
		if err == nil {
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("git operation in progress (%s)", marker)
			}
		}
	}
	status, err := r.Run(nil, "status", "--porcelain=v2", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("working tree has tracked or untracked changes")
	}
	_ = gitDir
	return nil
}

func (r Repo) IsAncestor(parent, child string) error {
	_, err := r.Run(nil, "merge-base", "--is-ancestor", "--", parent, child)
	return err
}

func (r Repo) UpdateRef(ref, newSHA, oldSHA string) error {
	args := []string{"update-ref", ref, newSHA}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	_, err := r.Run(nil, args...)
	return err
}

func (r Repo) DeleteRef(ref, oldSHA string) error {
	args := []string{"update-ref", "-d", ref}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	_, err := r.Run(nil, args...)
	return err
}

func (r Repo) RemoteURL(remote string) (string, error) {
	return r.Run(nil, "remote", "get-url", "--", remote)
}

func (r Repo) Remotes() ([]string, error) {
	out, err := r.Run(nil, "remote")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (r Repo) FetchContext(ctx context.Context, remote string, branches []string) error {
	args := []string{"fetch", "--no-tags", "--", remote}
	for _, branch := range branches {
		args = append(args, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch)
	}
	_, err := r.RunContext(ctx, nil, args...)
	return err
}

func (r Repo) PushLeaseContext(ctx context.Context, remote, branch, expected string) error {
	lease := "--force-with-lease=refs/heads/" + branch + ":" + expected
	_, err := r.RunContext(ctx, nil, "push", lease, "--", remote, "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

func (r Repo) RemoteHeadContext(ctx context.Context, remote, branch string) (string, error) {
	out, err := r.RunContext(ctx, nil, "ls-remote", "--heads", "--", remote, "refs/heads/"+branch)
	if err != nil || out == "" {
		return "", err
	}
	return strings.Fields(out)[0], nil
}

func (r Repo) RebaseContext(ctx context.Context, oldBase, newBase, branch, signingKey string) error {
	args := []string{"rebase", "--no-fork-point", "--reapply-cherry-picks", "--empty=keep", "--onto", newBase, oldBase, branch}
	if signingKey != "" {
		if signingKey == "default" {
			args = append(args, "--gpg-sign")
		} else {
			args = append(args, "--gpg-sign="+signingKey)
		}
	}
	_, err := r.RunContext(ctx, nil, args...)
	return err
}

func (r Repo) WorktreesForBranch(branch string) ([]string, error) {
	out, err := r.Run(nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var path string
	paths := make([]string, 0)
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func (r Repo) RebaseContinueContext(ctx context.Context) error {
	_, err := r.RunContext(ctx, []string{"GIT_EDITOR=true"}, "rebase", "--continue")
	return err
}

func (r Repo) RebaseAbortContext(ctx context.Context) error {
	_, err := r.RunContext(ctx, nil, "rebase", "--abort")
	return err
}

func (r Repo) RebaseActive() bool {
	for _, marker := range []string{"rebase-merge", "rebase-apply"} {
		path, err := r.GitPath(marker)
		if err == nil {
			if _, err := os.Stat(path); err == nil {
				return true
			}
		}
	}
	return false
}

func (r Repo) ConflictedFiles() []string {
	out, _ := r.Run(nil, "diff", "--name-only", "--diff-filter=U")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func (r Repo) Switch(branch string) error {
	_, err := r.Run(nil, "switch", "--", branch)
	return err
}

func (r Repo) SwitchCreate(branch, from string) error {
	_, err := r.Run(nil, "switch", "-c", branch, "--", from)
	return err
}
