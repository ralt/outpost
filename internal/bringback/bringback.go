// Package bringback implements the bring-back pipeline. See spec 07.
package bringback

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ralt/outpost/internal/config"
	"github.com/ralt/outpost/internal/ctl"
	"github.com/ralt/outpost/internal/logging"
	"github.com/ralt/outpost/internal/sshx"
	syncpkg "github.com/ralt/outpost/internal/sync"
)

type Pipeline struct {
	cfg config.Config
	log *slog.Logger
	ssh *sshx.Client
	eng *syncpkg.Engine

	mu      sync.Mutex
	running bool
}

func New(cfg config.Config, ssh *sshx.Client, eng *syncpkg.Engine, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg: cfg,
		log: logging.WithComponent(log, logging.CompSync).With("op", "bring-back"),
		ssh: ssh,
		eng: eng,
	}
}

func (p *Pipeline) Run(ctx context.Context, args ctl.BringBackArgs) (ctl.BringBackResult, error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return ctl.BringBackResult{}, ctl.NewError(ctl.CodeBusy, "another bring-back is already running")
	}
	p.running = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.running = false; p.mu.Unlock() }()

	cwd := args.Cwd
	if cwd == "" {
		return ctl.BringBackResult{}, ctl.NewError(ctl.CodeBadArgs, "cwd is required")
	}
	munged := syncpkg.MungedFromCwd(cwd)
	log := p.log.With("project", munged, "req", ctl.TraceID(ctx))

	// Preconditions.
	if !p.cfg.RemoteEnabled() {
		return ctl.BringBackResult{}, ctl.NewError(ctl.CodeNoRemote, "no remote configured")
	}
	sshState := p.ssh.Snapshot()
	if !sshState.Connected {
		return ctl.BringBackResult{}, ctl.NewErrorf(ctl.CodeDisconnected, "ssh down: %s", sshState.LastError)
	}
	pr, ok := p.eng.Project(munged)
	if !ok || pr.Owner != syncpkg.OwnerRemote {
		return ctl.BringBackResult{}, ctl.NewError(ctl.CodeNotSentAway, "project is not owned by the remote; run send-away first")
	}
	clean, err := localClean(cwd)
	if err != nil {
		return ctl.BringBackResult{}, ctl.NewErrorf(ctl.CodeInternal, "git status probe: %v", err)
	}
	if !clean {
		return ctl.BringBackResult{}, ctl.NewError(ctl.CodeLocalDirty, "local working tree has uncommitted changes; commit/stash by hand or edit .meta to forfeit remote work")
	}

	// Count session files that would be overwritten by remote.
	willOverwrite, _ := p.eng.SessionFilesFor(munged)
	// Confirmation gate.
	if !args.Confirmed {
		return ctl.BringBackResult{
			Project:       munged,
			NeedsConfirm:  true,
			WillOverwrite: len(willOverwrite),
		}, ctl.NewError(ctl.CodeNeedsConfirm, fmt.Sprintf("bring-back will overwrite %d local session files; re-invoke with confirmed=true", len(willOverwrite)))
	}

	// 1. Stop the remote agents.
	pids := []int{}
	for _, sm := range pr.Sessions {
		if sm.PID > 0 {
			pids = append(pids, sm.PID)
		}
	}
	if err := p.stopAgents(ctx, pids); err != nil {
		log.Warn("stop agents", "err", err)
	}

	// 2. Fetch + fast-forward.
	branch := pr.ActiveBranch
	if branch == "" {
		_, b, _ := branchAndHead(cwd)
		branch = b
	}
	commits, err := p.eng.FetchAndFastForward(ctx, munged, cwd, branch)
	if err != nil {
		return ctl.BringBackResult{}, ctl.NewErrorf(ctl.CodeInternal, "fetch: %v", err)
	}

	// 3. Ship remote's uncommitted delta back.
	hunks, untracked, err := p.applyRemoteDelta(ctx, cwd)
	if err != nil {
		return ctl.BringBackResult{}, err
	}

	// 4. sftp .jsonl files back, overwriting matching ids.
	sessions, err := p.eng.DownloadAllSessions(ctx, munged)
	if err != nil {
		return ctl.BringBackResult{}, ctl.NewErrorf(ctl.CodeInternal, "session pull: %v", err)
	}

	// 5. Flip owner → local, clear PIDs.
	p.eng.ClearSessions(munged)
	p.eng.SetOwner(munged, syncpkg.OwnerLocal)
	p.eng.Enqueue(munged) // re-attach streaming worker, etc.

	res := ctl.BringBackResult{
		Project:       munged,
		CommitsPulled: commits,
		Hunks:         hunks,
		Untracked:     untracked,
		Sessions:      sessions,
	}
	log.Info("done", "commits", commits, "hunks", hunks, "untracked", untracked, "sessions", sessions)
	return res, nil
}

func (p *Pipeline) stopAgents(ctx context.Context, pids []int) error {
	if len(pids) == 0 {
		return nil
	}
	var b strings.Builder
	for _, pid := range pids {
		fmt.Fprintf(&b, "kill %d 2>/dev/null; ", pid)
	}
	if _, _, err := p.ssh.Run(ctx, b.String()); err != nil {
		return err
	}
	// Wait up to 5s for graceful exit.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stillAlive := false
		for _, pid := range pids {
			cmd := fmt.Sprintf("kill -0 %d 2>/dev/null && echo yes || echo no", pid)
			so, _, err := p.ssh.Run(ctx, cmd)
			if err == nil && strings.TrimSpace(string(so)) == "yes" {
				stillAlive = true
				break
			}
		}
		if !stillAlive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	// SIGKILL the holdouts.
	var b2 strings.Builder
	for _, pid := range pids {
		fmt.Fprintf(&b2, "kill -9 %d 2>/dev/null; ", pid)
	}
	_, _, _ = p.ssh.Run(ctx, b2.String())
	return nil
}

// applyRemoteDelta runs `git diff --binary HEAD` on the remote and pipes it
// through `git apply --index` locally; same for untracked-not-ignored files
// via tar.
func (p *Pipeline) applyRemoteDelta(ctx context.Context, cwd string) (int, int, error) {
	// Patch.
	patch := &bytes.Buffer{}
	diffCmd := fmt.Sprintf("git -C %q diff --binary HEAD", cwd)
	stderr, err := p.ssh.RunStream(ctx, diffCmd, patch)
	if err != nil {
		return 0, 0, ctl.NewErrorf(ctl.CodeInternal, "remote diff: %s: %v", strings.TrimSpace(string(stderr)), err)
	}
	hunks := 0
	if patch.Len() > 0 {
		hunks = bytes.Count(patch.Bytes(), []byte("\n@@ "))
		apply := exec.Command("git", "-C", cwd, "apply", "--index", "--whitespace=nowarn")
		apply.Stdin = patch
		var se bytes.Buffer
		apply.Stderr = &se
		if err := apply.Run(); err != nil {
			return hunks, 0, ctl.NewErrorf(ctl.CodeApplyFailed, "local git apply: %s: %v", strings.TrimSpace(se.String()), err)
		}
	}

	// Untracked files via tar.
	tarBuf := &bytes.Buffer{}
	tarCmd := fmt.Sprintf("cd %q && git ls-files --others --exclude-standard -z | tar -c --null -T -", cwd)
	stderr2, err := p.ssh.RunStream(ctx, tarCmd, tarBuf)
	if err != nil {
		// `tar -c` over an empty list still produces a valid empty archive,
		// but if the project has no untracked files we just see "" — treat
		// non-zero exits with an empty list as ok.
		if tarBuf.Len() == 0 {
			return hunks, 0, nil
		}
		return hunks, 0, ctl.NewErrorf(ctl.CodeInternal, "remote tar: %s: %v", strings.TrimSpace(string(stderr2)), err)
	}
	if tarBuf.Len() == 0 {
		return hunks, 0, nil
	}
	tarData := tarBuf.Bytes()
	untar := exec.Command("tar", "-x", "-C", cwd)
	untar.Stdin = bytes.NewReader(tarData)
	var use bytes.Buffer
	untar.Stderr = &use
	if err := untar.Run(); err != nil {
		return hunks, 0, ctl.NewErrorf(ctl.CodeApplyFailed, "local tar -x: %s: %v", strings.TrimSpace(use.String()), err)
	}
	count, _ := countTarFiles(tarData)
	return hunks, count, nil
}

// countTarFiles returns the number of regular-file entries in a tar stream.
// Directory entries don't count: git ls-files lists files, but `tar -c`
// inserts the containing dirs automatically and we don't want to count those.
func countTarFiles(b []byte) (int, error) {
	r := tar.NewReader(bytes.NewReader(b))
	n := 0
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeSymlink, tar.TypeLink:
			n++
		}
	}
}

func localClean(cwd string) (bool, error) {
	cmd := exec.Command("git", "-C", cwd, "status", "--porcelain")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out.Bytes())) == 0, nil
}

func branchAndHead(cwd string) (string, string, error) {
	bb, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", "", err
	}
	branch := strings.TrimSpace(string(bb))
	if branch == "HEAD" {
		branch = ""
	}
	sb, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
	if err != nil {
		return branch, "", err
	}
	return branch, strings.TrimSpace(string(sb)), nil
}

// silence unused imports while iterating
var _ = errors.New
var _ = filepath.Join
