// Package sendaway implements the send-away pipeline. See spec 07.
package sendaway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Pipeline owns send-away state. It guards against concurrent invocations and
// holds references to the rest of the daemon's wiring.
type Pipeline struct {
	cfg config.Config
	log *slog.Logger
	ssh *sshx.Client
	eng *syncpkg.Engine

	mu  sync.Mutex
	running bool
}

func New(cfg config.Config, ssh *sshx.Client, eng *syncpkg.Engine, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg: cfg,
		log: logging.WithComponent(log, logging.CompSync).With("op", "send-away"),
		ssh: ssh,
		eng: eng,
	}
}

// Run executes the full send-away flow synchronously.
func (p *Pipeline) Run(ctx context.Context, args ctl.SendAwayArgs) (ctl.SendAwayResult, error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return ctl.SendAwayResult{}, ctl.NewError(ctl.CodeBusy, "another send-away is already running")
	}
	p.running = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.running = false; p.mu.Unlock() }()

	cwd := args.Cwd
	if cwd == "" {
		return ctl.SendAwayResult{}, ctl.NewError(ctl.CodeBadArgs, "cwd is required")
	}
	munged := syncpkg.MungedFromCwd(cwd)
	log := p.log.With("project", munged, "req", ctl.TraceID(ctx))

	// Preconditions.
	if !p.cfg.RemoteEnabled() {
		return ctl.SendAwayResult{}, ctl.NewError(ctl.CodeNoRemote, "set [remote] host in "+p.cfg.SourcePath)
	}
	sshState := p.ssh.Snapshot()
	if !sshState.Connected {
		if strings.HasPrefix(sshState.LastError, "HOME_MISMATCH") {
			return ctl.SendAwayResult{}, ctl.NewError(ctl.CodeHomeMismatch, sshState.LastError)
		}
		return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeDisconnected, "ssh to %s is down: %s", p.cfg.Remote.Host, sshState.LastError)
	}
	existing, _ := p.eng.Project(munged)
	if existing.Owner == syncpkg.OwnerRemote {
		return ctl.SendAwayResult{}, ctl.NewError(ctl.CodeAlreadySent, "project already sent away (owner=remote); run bring-back")
	}

	// Working tree must be a real git repo with at least one commit.
	gitProbe := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree")
	if err := gitProbe.Run(); err != nil {
		return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeNoGit, "cwd %s is not a git repo (run git init)", cwd)
	}
	if !hasHead(cwd) {
		return ctl.SendAwayResult{}, ctl.NewError(ctl.CodeNoCommits, "the current branch has no commits — commit at least once")
	}
	if op := midOp(cwd); op != "" {
		return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeInProgressOp, "git is mid-%s; finish or abort it first", op)
	}

	branch, headSHA, err := branchAndHead(cwd)
	if err != nil {
		return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeInternal, "git introspect: %v", err)
	}

	// Refresh the engine's view with what we just verified: the project IS a
	// git repo at this exact cwd. The discovery path can't recover '.' in
	// path components from the munged name, so it may have marked this as
	// non-applicable. The supplied cwd is authoritative.
	p.eng.RegisterProject(munged, cwd, branch)
	existing, _ = p.eng.Project(munged)

	// Bootstrap state check. The engine may have never seen this project yet.
	bootstrapped := false
	state := existing.RemoteState
	if state == "" || state == syncpkg.StateClonePending || state == syncpkg.StateCloneFailed || state == syncpkg.StateNotApplicable {
		log.Info("inline bootstrap (new project)")
		if err := p.eng.EnsureRemoteScaffold(ctx); err != nil {
			return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeInternal, "remote scaffold: %v", err)
		}
		// Use the engine's exposed bootstrap helper via Enqueue path is async,
		// so call the public helper directly.
		if err := p.eng.BootstrapNow(ctx, munged, cwd); err != nil {
			return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeCloneFailed, "bootstrap: %v", err)
		}
		bootstrapped = true
	}

	// 0. Detach the remote worktree's HEAD so the catch-up push isn't blocked
	//    by 'branch is currently checked out' on refs/heads/<branch>. The
	//    reconcile step below reattaches before the agent launches.
	if !bootstrapped {
		detach := fmt.Sprintf("git -C %q -c advice.detachedHead=false switch --detach 2>/dev/null || true", cwd)
		_, _, _ = p.ssh.Run(ctx, detach)
	}

	// 1. Catch-up mirror push (usually a no-op).
	refsPushed := 0
	if err := p.eng.MirrorPushNow(ctx, munged, cwd); err != nil {
		return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeRemoteReject, "catch-up push: %v", err)
	}
	refsPushed++ // we don't track per-ref counts cheaply; report 1+ for "pushed".

	// 2. Drain + pause session stream.
	p.eng.PauseStreaming(ctx, munged)

	// On inline bootstrap, ship every session .jsonl fresh.
	if bootstrapped {
		if err := p.eng.UploadAllSessionsNow(ctx, munged); err != nil {
			log.Warn("session upload during bootstrap", "err", err)
		}
	}

	// 3. Worktree reconcile: switch + reset --hard on the remote.
	if err := p.reconcileWorktree(ctx, cwd, branch, headSHA); err != nil {
		return ctl.SendAwayResult{}, err
	}

	// 4. Ship uncommitted delta (diff --binary HEAD + tar of untracked-not-ignored).
	hunks, err := p.shipPatch(ctx, cwd)
	if err != nil {
		return ctl.SendAwayResult{}, err
	}
	var untracked int
	if p.cfg.Sync.SendUntracked {
		untracked, err = p.shipUntracked(ctx, cwd)
		if err != nil {
			return ctl.SendAwayResult{}, err
		}
	}

	// 5. Flip owner → remote *before* launching agents so a crash here leaves
	//    the project in an unambiguous state.
	p.eng.SetOwner(munged, syncpkg.OwnerRemote)

	// 6. Launch one headless agent per session.
	sessions, err := p.eng.SessionFilesFor(munged)
	if err != nil {
		return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeInternal, "session enum: %v", err)
	}
	logDir := p.eng.RemoteLogDir(munged)
	if _, se, err := p.ssh.Run(ctx, fmt.Sprintf("mkdir -p %q", logDir)); err != nil {
		return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeInternal, "remote log mkdir: %s: %v", strings.TrimSpace(string(se)), err)
	}
	agents := []ctl.AgentInfo{}
	for _, f := range sessions {
		id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		// If args.Session specified and this isn't it, skip — caller wants a single session.
		if args.Session != "" && args.Session != id {
			continue
		}
		ai, state, err := p.launchOrUpdate(ctx, cwd, munged, id, logDir)
		if err != nil {
			return ctl.SendAwayResult{}, ctl.NewErrorf(ctl.CodeRemoteMissing, "launch %s: %v", id, err)
		}
		ai.State = state
		agents = append(agents, ai)
	}

	res := ctl.SendAwayResult{
		Project:          munged,
		Agents:           agents,
		MirrorPushRefs:   refsPushed,
		UncommittedHunks: hunks,
		Untracked:        untracked,
		Branch:           branch,
		Head:             shortSHA(headSHA),
	}
	if bootstrapped {
		res.BootstrapState = "BOOTSTRAPPED_INLINE"
	}
	log.Info("done", "agents", len(agents), "hunks", hunks, "untracked", untracked, "dur", time.Since(time.Now()).String())
	return res, nil
}

// reconcileWorktree runs `git switch <branch>` + `git reset --hard <sha>` on
// the remote worktree, respecting sync.on_conflict.
func (p *Pipeline) reconcileWorktree(ctx context.Context, cwd, branch, sha string) error {
	if branch == "" {
		// Detached HEAD locally — point remote at the sha directly.
		cmd := fmt.Sprintf("git -C %q -c advice.detachedHead=false switch --detach %s", cwd, sha)
		if _, se, err := p.ssh.Run(ctx, cmd); err != nil {
			return ctl.NewErrorf(ctl.CodeRemoteReject, "switch --detach: %s: %v", strings.TrimSpace(string(se)), err)
		}
		return nil
	}
	switchCmd := fmt.Sprintf("git -C %q switch %s 2>/dev/null || git -C %q switch -c %s", cwd, branch, cwd, branch)
	if _, se, err := p.ssh.Run(ctx, switchCmd); err != nil {
		return ctl.NewErrorf(ctl.CodeRemoteReject, "switch: %s: %v", strings.TrimSpace(string(se)), err)
	}
	resetCmd := fmt.Sprintf("git -C %q reset --hard %s", cwd, sha)
	if p.cfg.Sync.OnConflict == "abort" {
		// Detect remote dirty state and refuse to clobber.
		clean := fmt.Sprintf("git -C %q status --porcelain", cwd)
		so, _, err := p.ssh.Run(ctx, clean)
		if err == nil && len(bytes.TrimSpace(so)) > 0 {
			return ctl.NewError(ctl.CodeRemoteReject, "remote worktree has uncommitted changes; set sync.on_conflict=local-wins to override")
		}
	}
	if _, se, err := p.ssh.Run(ctx, resetCmd); err != nil {
		return ctl.NewErrorf(ctl.CodeRemoteReject, "reset --hard: %s: %v", strings.TrimSpace(string(se)), err)
	}
	return nil
}

// shipPatch generates `git diff --binary HEAD` locally and pipes it through
// `git apply --index --whitespace=nowarn` on the remote. Returns hunk count.
func (p *Pipeline) shipPatch(ctx context.Context, cwd string) (int, error) {
	patch := &bytes.Buffer{}
	cmd := exec.Command("git", "-C", cwd, "diff", "--binary", "HEAD")
	cmd.Stdout = patch
	var se bytes.Buffer
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		return 0, ctl.NewErrorf(ctl.CodeInternal, "git diff: %v (%s)", err, strings.TrimSpace(se.String()))
	}
	if patch.Len() == 0 {
		return 0, nil
	}
	hunks := bytes.Count(patch.Bytes(), []byte("\n@@ "))
	applyCmd := fmt.Sprintf("git -C %q apply --index --whitespace=nowarn", cwd)
	_, errOut, err := p.ssh.RunStdin(ctx, applyCmd, bytes.NewReader(patch.Bytes()))
	if err != nil {
		return hunks, ctl.NewErrorf(ctl.CodeApplyFailed, "remote git apply: %s: %v", strings.TrimSpace(string(errOut)), err)
	}
	return hunks, nil
}

// shipUntracked tars untracked-not-ignored files and pipes into tar -x on the
// remote. Returns the file count.
func (p *Pipeline) shipUntracked(ctx context.Context, cwd string) (int, error) {
	listCmd := exec.Command("git", "-C", cwd, "ls-files", "--others", "--exclude-standard", "-z")
	var list bytes.Buffer
	listCmd.Stdout = &list
	var se bytes.Buffer
	listCmd.Stderr = &se
	if err := listCmd.Run(); err != nil {
		return 0, ctl.NewErrorf(ctl.CodeInternal, "git ls-files: %v (%s)", err, strings.TrimSpace(se.String()))
	}
	if list.Len() == 0 {
		return 0, nil
	}
	count := bytes.Count(list.Bytes(), []byte{0})
	// Pipe list through tar -c --null -T - and on remote, tar -x.
	tarOut := &bytes.Buffer{}
	tarCmd := exec.Command("tar", "-c", "--null", "-T", "-")
	tarCmd.Dir = cwd
	tarCmd.Stdin = bytes.NewReader(list.Bytes())
	tarCmd.Stdout = tarOut
	var tse bytes.Buffer
	tarCmd.Stderr = &tse
	if err := tarCmd.Run(); err != nil {
		return count, ctl.NewErrorf(ctl.CodeInternal, "local tar: %v (%s)", err, strings.TrimSpace(tse.String()))
	}
	remoteCmd := fmt.Sprintf("tar -x -C %q", cwd)
	_, errOut, err := p.ssh.RunStdin(ctx, remoteCmd, bytes.NewReader(tarOut.Bytes()))
	if err != nil {
		return count, ctl.NewErrorf(ctl.CodeApplyFailed, "remote tar -x: %s: %v", strings.TrimSpace(string(errOut)), err)
	}
	return count, nil
}

// launchOrUpdate runs (or notices an already-running) headless claude agent
// for a session id. Returns AgentInfo and a state tag ("started"|"updated").
func (p *Pipeline) launchOrUpdate(ctx context.Context, cwd, munged, id, logDir string) (ctl.AgentInfo, string, error) {
	existing, _ := p.eng.Project(munged)
	if sm, ok := existing.Sessions[id]; ok && sm.PID > 0 {
		// Check whether the recorded PID is still alive.
		if alive, _ := p.isPIDAlive(ctx, sm.PID); alive {
			return ctl.AgentInfo{Session: id, PID: sm.PID, Log: sm.Log}, "updated", nil
		}
	}

	logFile := filepath.Join(logDir, id+".log")
	tools := p.cfg.Remote.AllowedTools
	prompt := p.cfg.Remote.ContinuePrompt
	bin := p.cfg.Remote.ClaudeBin
	// Quote each arg minimally. We construct via printf-quoting via %q in Go's
	// fmt for path-like strings; the prompt is user-controlled so shell-escape
	// it with single quotes.
	promptArg := singleQuote(prompt)
	toolsArg := singleQuote(tools)
	// The explicit `( ... )` is load-bearing. Without it, `cd && nohup Y & echo $!`
	// parses as `(cd && nohup Y) & echo $!`, creating an implicit subshell that
	// waits foreground for nohup — and whose stdout is the SSH pipe. The session
	// never closes and ssh.Run hangs. With the grouping below, the subshell
	// returns immediately after echo $!.
	cmd := fmt.Sprintf(
		"cd %q && ( nohup %s -p %s --resume %s --allowedTools %s > %q 2>&1 < /dev/null & echo $! )",
		cwd, bin, promptArg, id, toolsArg, logFile,
	)
	so, se, err := p.ssh.Run(ctx, cmd)
	if err != nil {
		if strings.Contains(string(se), "not found") {
			return ctl.AgentInfo{}, "", ctl.NewErrorf(ctl.CodeRemoteMissing, "claude binary not found on remote: %s", strings.TrimSpace(string(se)))
		}
		return ctl.AgentInfo{}, "", fmt.Errorf("ssh nohup: %s: %w", strings.TrimSpace(string(se)), err)
	}
	pidStr := strings.TrimSpace(string(so))
	pid := 0
	fmt.Sscanf(pidStr, "%d", &pid)
	if pid == 0 {
		return ctl.AgentInfo{}, "", fmt.Errorf("no PID returned from remote (got %q)", pidStr)
	}
	sm := syncpkg.SessionMeta{PID: pid, Log: logFile, StartedAt: time.Now().UTC()}
	p.eng.RegisterSession(munged, id, sm)
	return ctl.AgentInfo{Session: id, PID: pid, Log: logFile}, "started", nil
}

func (p *Pipeline) isPIDAlive(ctx context.Context, pid int) (bool, error) {
	cmd := fmt.Sprintf("kill -0 %d 2>/dev/null && echo yes || echo no", pid)
	so, _, err := p.ssh.Run(ctx, cmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(so)) == "yes", nil
}

func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func hasHead(cwd string) bool {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--verify", "HEAD")
	return cmd.Run() == nil
}

func midOp(cwd string) string {
	for _, m := range []struct {
		Name string
		Path string
	}{
		{"rebase-apply", ".git/rebase-apply"},
		{"rebase-merge", ".git/rebase-merge"},
		{"merge", ".git/MERGE_HEAD"},
		{"bisect", ".git/BISECT_LOG"},
		{"cherry-pick", ".git/CHERRY_PICK_HEAD"},
		{"revert", ".git/REVERT_HEAD"},
	} {
		path := filepath.Join(cwd, m.Path)
		cmd := exec.Command("test", "-e", path)
		if cmd.Run() == nil {
			return m.Name
		}
	}
	return ""
}

func branchAndHead(cwd string) (string, string, error) {
	branch := ""
	if out, err := runGit(cwd, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		branch = strings.TrimSpace(string(out))
		if branch == "HEAD" {
			branch = ""
		}
	}
	sha := ""
	out, err := runGit(cwd, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	sha = strings.TrimSpace(string(out))
	if sha == "" {
		return "", "", errors.New("HEAD has no commit")
	}
	return branch, sha, nil
}

func runGit(cwd string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		return so.Bytes(), fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(se.String()))
	}
	return so.Bytes(), nil
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
