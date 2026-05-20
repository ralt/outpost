package sync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// bootstrap is the first-time setup for a project on the remote:
//
//   1. mkdir -p <remote>/repos /logs /.meta
//   2. git init --bare <remote>/repos/<munged>.git
//   3. go-git mirror push   → all refs into the bare
//   4. git -C <bare> worktree add <cwd> <branch>
//
// Idempotent: rerunning either fast-forwards or no-ops.
func (e *Engine) bootstrap(ctx context.Context, munged, cwd string) error {
	if !e.cfg.RemoteEnabled() {
		return errors.New("remote not configured")
	}
	if err := e.EnsureRemoteScaffold(ctx); err != nil {
		return err
	}
	bare := e.RemoteBarePath(munged)

	// 2. git init --bare (-q for cleanliness; subsequent runs no-op).
	initCmd := fmt.Sprintf("if [ ! -d %q ]; then git init --bare -q %q; fi", bare, bare)
	if _, se, err := e.ssh.Run(ctx, initCmd); err != nil {
		return fmt.Errorf("init bare: %s: %w", strings.TrimSpace(string(se)), err)
	}

	// 3. mirror push.
	if err := e.mirrorPush(ctx, munged, cwd); err != nil {
		return err
	}

	// 4. worktree add at the reproduced path. If <cwd> already exists with the
	//    bare repo's worktree pointer, this is a no-op.
	branch := defaultBranch(cwd)
	hasWT := fmt.Sprintf("git -C %q worktree list --porcelain | grep -F %q >/dev/null", bare, "worktree "+cwd)
	if _, _, err := e.ssh.Run(ctx, hasWT); err != nil {
		// Worktree not found — add it.
		// Make sure the parent dir exists; bare's worktree-add requires absent target.
		parent := filepath.Dir(cwd)
		mkparent := fmt.Sprintf("mkdir -p %q", parent)
		if _, se, err := e.ssh.Run(ctx, mkparent); err != nil {
			return fmt.Errorf("mkdir parent: %s: %w", strings.TrimSpace(string(se)), err)
		}
		// Use -f to overwrite if a previous attempt left a stale state. After
		// the add, detach HEAD so future mirror pushes to <branch> aren't
		// blocked by 'branch is currently checked out' — send-away's reconcile
		// step reattaches before launching the agent.
		add := fmt.Sprintf(
			"if [ -e %q ]; then rm -rf %q; fi && git -C %q worktree add -f %q %s && git -C %q -c advice.detachedHead=false switch --detach",
			cwd, cwd, bare, cwd, branch, cwd,
		)
		if _, se, err := e.ssh.Run(ctx, add); err != nil {
			return fmt.Errorf("worktree add: %s: %w", strings.TrimSpace(string(se)), err)
		}
	}
	return nil
}

func defaultBranch(cwd string) string {
	_, branch, _ := inspectWorkingTree(cwd)
	if branch == "" {
		return "main"
	}
	return branch
}
