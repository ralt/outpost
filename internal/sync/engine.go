package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/sftp"

	"github.com/ralt/outpost/internal/config"
	"github.com/ralt/outpost/internal/fusefs"
	"github.com/ralt/outpost/internal/logging"
	"github.com/ralt/outpost/internal/sshx"
)

// Engine ties the project watcher, the background scheduler, and the session
// streamer together. One per daemon.
type Engine struct {
	cfg     config.Config
	log     *slog.Logger
	ssh     *sshx.Client
	meta    *MetaStore

	projectsDir string // <backing>/projects
	metaDir     string // <backing>/.. /.meta
	logsDir     string // local logs dir mirror (we mainly use remote)
	dataDir     string // <backing> parent (~/.local/share/outpost)

	mu       sync.RWMutex
	projects map[string]*projectState

	dispatch chan event
	wg       sync.WaitGroup
	stopCh   chan struct{}
}

type projectState struct {
	mu       sync.RWMutex
	p        Project
	streamer *streamer
}

func (s *projectState) snapshot() Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.p
	if s.p.Sessions != nil {
		out.Sessions = make(map[string]SessionMeta, len(s.p.Sessions))
		for k, v := range s.p.Sessions {
			out.Sessions[k] = v
		}
	}
	return out
}

type event struct {
	kind   string // "discovered" | "refresh"
	munged string
}

// New constructs an Engine but does not start it.
func New(cfg config.Config, ssh *sshx.Client, log *slog.Logger) *Engine {
	backing := cfg.Paths.Backing
	parent := filepath.Dir(backing)
	return &Engine{
		cfg:         cfg,
		log:         logging.WithComponent(log, logging.CompSync),
		ssh:         ssh,
		meta:        NewMetaStore(filepath.Join(parent, ".meta")),
		projectsDir: filepath.Join(backing, "projects"),
		metaDir:     filepath.Join(parent, ".meta"),
		logsDir:     filepath.Join(parent, "logs"),
		dataDir:     parent,
		projects:    map[string]*projectState{},
		dispatch:    make(chan event, 64),
		stopCh:      make(chan struct{}),
	}
}

// MetaStore returns the engine's persistent metadata store.
func (e *Engine) MetaStore() *MetaStore { return e.meta }

// Project returns a snapshot of the requested project, or false if unknown.
func (e *Engine) Project(munged string) (Project, bool) {
	e.mu.RLock()
	st, ok := e.projects[munged]
	e.mu.RUnlock()
	if !ok {
		return Project{}, false
	}
	return st.snapshot(), true
}

// Projects returns snapshots of every known project.
func (e *Engine) Projects() []Project {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Project, 0, len(e.projects))
	for _, st := range e.projects {
		out = append(out, st.snapshot())
	}
	return out
}

// Start begins watching for new projects, loads persisted metadata, and kicks
// off the scheduler tick.
func (e *Engine) Start(ctx context.Context) error {
	if err := os.MkdirAll(e.projectsDir, 0o700); err != nil {
		return fmt.Errorf("sync: mkdir projects: %w", err)
	}
	if err := os.MkdirAll(e.metaDir, 0o700); err != nil {
		return fmt.Errorf("sync: mkdir meta: %w", err)
	}
	if err := os.MkdirAll(e.logsDir, 0o700); err != nil {
		return fmt.Errorf("sync: mkdir logs: %w", err)
	}

	// Hydrate from disk.
	metas, err := e.meta.LoadAll()
	if err != nil {
		e.log.Warn("meta load", "err", err)
	}
	for _, m := range metas {
		st := &projectState{p: Project{
			Munged:         m.Munged,
			Path:           m.Path,
			IsGit:          m.IsGit,
			Owner:          m.Owner,
			RemoteState:    m.RemoteState,
			Streaming:      m.Streaming,
			ActiveBranch:   m.ActiveBranch,
			LastMirrorPush: m.LastMirrorPush,
			LastError:      m.LastError,
			Sessions:       cloneSessions(m.Sessions),
		}}
		e.projects[m.Munged] = st
	}

	// Seed from on-disk project dirs (in case a project exists with no meta yet).
	if entries, err := os.ReadDir(e.projectsDir); err == nil {
		for _, en := range entries {
			if !en.IsDir() {
				continue
			}
			if _, ok := e.projects[en.Name()]; ok {
				continue
			}
			e.upsertProject(en.Name())
		}
	}

	e.wg.Add(2)
	go e.runWatcher(ctx)
	go e.runWorker(ctx)
	if e.cfg.Sync.BackgroundInterval > 0 {
		e.wg.Add(1)
		go e.runScheduler(ctx)
	}

	// Trigger an initial pass for everything known.
	go func() {
		e.mu.RLock()
		names := make([]string, 0, len(e.projects))
		for n := range e.projects {
			names = append(names, n)
		}
		e.mu.RUnlock()
		for _, n := range names {
			select {
			case e.dispatch <- event{kind: "discovered", munged: n}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

// Stop signals shutdown and waits for goroutines.
func (e *Engine) Stop() {
	select {
	case <-e.stopCh:
		return
	default:
		close(e.stopCh)
	}
	e.wg.Wait()
	// Tear down active streamers.
	e.mu.RLock()
	for _, st := range e.projects {
		if st.streamer != nil {
			st.streamer.Stop()
		}
	}
	e.mu.RUnlock()
}

func cloneSessions(in map[string]SessionMeta) map[string]SessionMeta {
	if in == nil {
		return nil
	}
	out := make(map[string]SessionMeta, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (e *Engine) upsertProject(munged string) *projectState {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.projects[munged]
	if ok {
		return st
	}
	cwd := CwdFromMunged(munged)
	st = &projectState{p: Project{
		Munged:      munged,
		Path:        cwd,
		Owner:       OwnerLocal,
		RemoteState: StateClonePending,
	}}
	e.projects[munged] = st
	return st
}

// ── Watcher: fsnotify on <backing>/projects ─────────────────────────

func (e *Engine) runWatcher(ctx context.Context) {
	defer e.wg.Done()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		e.log.Error("fsnotify init", "err", err)
		return
	}
	defer w.Close()
	if err := w.Add(e.projectsDir); err != nil {
		e.log.Error("fsnotify watch", "dir", e.projectsDir, "err", err)
		return
	}
	debounce := e.cfg.Sync.DiscoveryDebounce
	if debounce <= 0 {
		debounce = 5 * time.Second
	}
	pending := map[string]*time.Timer{}
	pendingMu := sync.Mutex{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			// Only care about new directories appearing directly under projectsDir.
			if filepath.Dir(ev.Name) != e.projectsDir {
				continue
			}
			if ev.Op&fsnotify.Create == 0 {
				continue
			}
			munged := filepath.Base(ev.Name)
			pendingMu.Lock()
			if t, exists := pending[munged]; exists {
				t.Reset(debounce)
			} else {
				m := munged
				pending[m] = time.AfterFunc(debounce, func() {
					pendingMu.Lock()
					delete(pending, m)
					pendingMu.Unlock()
					e.log.Info("project discovered", "project", m)
					select {
					case e.dispatch <- event{kind: "discovered", munged: m}:
					case <-e.stopCh:
					}
				})
			}
			pendingMu.Unlock()
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			e.log.Warn("fsnotify error", "err", err)
		}
	}
}

// ── Scheduler: periodic mirror push ─────────────────────────────────

func (e *Engine) runScheduler(ctx context.Context) {
	defer e.wg.Done()
	t := time.NewTicker(e.cfg.Sync.BackgroundInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-t.C:
			e.mu.RLock()
			names := make([]string, 0, len(e.projects))
			for n, st := range e.projects {
				p := st.snapshot()
				if p.Owner == OwnerLocal {
					names = append(names, n)
				}
			}
			e.mu.RUnlock()
			for _, n := range names {
				select {
				case e.dispatch <- event{kind: "refresh", munged: n}:
				case <-e.stopCh:
					return
				}
			}
		}
	}
}

// ── Worker: serial processing of project events ─────────────────────

func (e *Engine) runWorker(ctx context.Context) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case ev := <-e.dispatch:
			e.process(ctx, ev)
		}
	}
}

func (e *Engine) process(ctx context.Context, ev event) {
	taskID := logging.NewTraceID()
	log := e.log.With("task", taskID, "project", ev.munged)

	st := e.upsertProject(ev.munged)

	// Reconstruct path and verify the working tree.
	cwd := st.snapshot().Path
	if cwd == "" {
		cwd = CwdFromMunged(ev.munged)
	}
	isGit, branch, _ := inspectWorkingTree(cwd)
	e.updateProject(ev.munged, func(p *Project) {
		p.Path = cwd
		p.IsGit = isGit
		if branch != "" {
			p.ActiveBranch = branch
		}
		if !isGit {
			p.RemoteState = StateNotApplicable
		}
	})

	if !isGit {
		log.Info("non-git project, skipping remote sync")
		e.persist(ev.munged)
		return
	}
	if !e.cfg.RemoteEnabled() {
		log.Debug("remote disabled — keeping local-only")
		e.persist(ev.munged)
		return
	}

	// Don't push to a remote where the user has handed work over.
	owner := st.snapshot().Owner
	if owner == OwnerRemote {
		log.Debug("owner=remote — skipping background mirror push")
		return
	}

	// Cheap ssh-connected check; bail fast if not connected.
	snap := e.ssh.Snapshot()
	if !snap.Connected {
		e.updateProject(ev.munged, func(p *Project) { p.LastError = "ssh disconnected" })
		log.Debug("ssh disconnected; will retry later")
		return
	}

	// Bootstrap if needed.
	state := st.snapshot().RemoteState
	if state == "" || state == StateClonePending || state == StateCloneFailed {
		if err := e.bootstrap(ctx, ev.munged, cwd); err != nil {
			e.updateProject(ev.munged, func(p *Project) {
				p.RemoteState = StateCloneFailed
				p.LastError = err.Error()
			})
			log.Warn("bootstrap failed", "err", err)
			e.persist(ev.munged)
			return
		}
		e.updateProject(ev.munged, func(p *Project) {
			p.RemoteState = StateSynced
			p.LastError = ""
			p.LastMirrorPush = time.Now()
		})
		log.Info("bootstrap complete")
	} else {
		if err := e.mirrorPush(ctx, ev.munged, cwd); err != nil {
			e.updateProject(ev.munged, func(p *Project) { p.LastError = err.Error() })
			log.Warn("mirror push failed", "err", err)
			e.persist(ev.munged)
			return
		}
		e.updateProject(ev.munged, func(p *Project) {
			p.LastMirrorPush = time.Now()
			p.LastError = ""
		})
		log.Debug("mirror push ok")
	}

	// Streamer up.
	e.ensureStreamer(ctx, ev.munged)
	e.persist(ev.munged)
}

func (e *Engine) updateProject(munged string, mut func(p *Project)) {
	e.mu.Lock()
	st, ok := e.projects[munged]
	if !ok {
		st = &projectState{p: Project{Munged: munged}}
		e.projects[munged] = st
	}
	e.mu.Unlock()
	st.mu.Lock()
	mut(&st.p)
	st.mu.Unlock()
}

func (e *Engine) persist(munged string) {
	e.mu.RLock()
	st, ok := e.projects[munged]
	e.mu.RUnlock()
	if !ok {
		return
	}
	p := st.snapshot()
	m := &Meta{
		Munged:         p.Munged,
		Path:           p.Path,
		IsGit:          p.IsGit,
		Owner:          p.Owner,
		RemoteState:    p.RemoteState,
		Streaming:      p.Streaming,
		ActiveBranch:   p.ActiveBranch,
		OriginCwd:      p.Path,
		LastMirrorPush: p.LastMirrorPush,
		LastError:      p.LastError,
		Sessions:       cloneSessions(p.Sessions),
	}
	if err := e.meta.Save(m); err != nil {
		e.log.Warn("meta save", "project", munged, "err", err)
	}
}

// SessionFilesFor returns every .jsonl path under <backing>/projects/<munged>/.
func (e *Engine) SessionFilesFor(munged string) ([]string, error) {
	dir := filepath.Join(e.projectsDir, munged)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// LatestSession returns the newest .jsonl basename (without extension), or
// empty if none exists.
func (e *Engine) LatestSession(munged string) (string, error) {
	files, err := e.SessionFilesFor(munged)
	if err != nil {
		return "", err
	}
	var newest string
	var newestMod time.Time
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		if st.ModTime().After(newestMod) {
			newest = filepath.Base(f)
			newestMod = st.ModTime()
		}
	}
	if newest == "" {
		return "", nil
	}
	return strings.TrimSuffix(newest, ".jsonl"), nil
}

// SetOwner is used by send-away/bring-back to flip ownership.
func (e *Engine) SetOwner(munged, owner string) {
	e.updateProject(munged, func(p *Project) { p.Owner = owner })
	e.persist(munged)
}

// RegisterSession records a launched headless agent.
func (e *Engine) RegisterSession(munged, id string, sm SessionMeta) {
	e.updateProject(munged, func(p *Project) {
		if p.Sessions == nil {
			p.Sessions = map[string]SessionMeta{}
		}
		p.Sessions[id] = sm
	})
	e.persist(munged)
}

// ClearSessions drops the recorded agent table for a project.
func (e *Engine) ClearSessions(munged string) {
	e.updateProject(munged, func(p *Project) { p.Sessions = nil })
	e.persist(munged)
}

// DataDir is the parent of the backing dir; the remote layout reproduces this
// under the remote $HOME at the same relative location.
func (e *Engine) DataDir() string { return e.dataDir }

// RemoteDataDir computes the parallel path on the remote: same as local (since
// $HOME matches).
func (e *Engine) RemoteDataDir() string { return e.dataDir }

// RemoteBarePath returns the bare repo location on the remote for a project.
func (e *Engine) RemoteBarePath(munged string) string {
	return filepath.Join(e.dataDir, "repos", munged+".git")
}

// RemoteLogDir returns the per-project agent log dir on the remote.
func (e *Engine) RemoteLogDir(munged string) string {
	return filepath.Join(e.dataDir, "logs", munged)
}

// SessionDir returns the local backing directory holding session files.
func (e *Engine) SessionDir(munged string) string {
	return filepath.Join(e.projectsDir, munged)
}

// EnsureRemoteScaffold creates ~/.local/share/outpost/{repos,logs,.meta} on the
// remote if missing. Cheap; safe to call repeatedly.
func (e *Engine) EnsureRemoteScaffold(ctx context.Context) error {
	cmd := fmt.Sprintf("mkdir -p %q %q %q", filepath.Join(e.dataDir, "repos"), filepath.Join(e.dataDir, "logs"), filepath.Join(e.dataDir, ".meta"))
	_, se, err := e.ssh.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("remote scaffold: %s: %w", strings.TrimSpace(string(se)), err)
	}
	return nil
}

// Sink methods — Engine implements fusefs.Sink so the FUSE layer can hand it
// session writes directly.

func (e *Engine) OnWrite(ev fusefs.WriteEvent) {
	e.mu.RLock()
	st, ok := e.projects[ev.Munged]
	e.mu.RUnlock()
	if !ok || st.streamer == nil {
		return
	}
	st.streamer.OnWrite(ev)
}

func (e *Engine) OnSession(ev fusefs.SessionLifecycleEvent) {
	e.mu.RLock()
	st, ok := e.projects[ev.Munged]
	e.mu.RUnlock()
	if !ok {
		// First-write project — bring it into the index so the watcher
		// + worker can bootstrap it.
		select {
		case e.dispatch <- event{kind: "discovered", munged: ev.Munged}:
		default:
		}
		return
	}
	if st.streamer != nil {
		st.streamer.OnSession(ev)
	}
}

// ensureStreamer creates the per-project streaming worker if absent.
func (e *Engine) ensureStreamer(ctx context.Context, munged string) {
	e.mu.Lock()
	st, ok := e.projects[munged]
	e.mu.Unlock()
	if !ok {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.streamer != nil {
		return
	}
	if st.p.Owner != OwnerLocal {
		return
	}
	srv := newStreamer(e, munged, e.log.With("project", munged))
	st.streamer = srv
	st.p.Streaming = true
	srv.Start(ctx)
}

// PauseStreaming flushes pending writes for a project and stops its streamer.
// Used by send-away before flipping owner=remote.
func (e *Engine) PauseStreaming(ctx context.Context, munged string) {
	e.mu.RLock()
	st, ok := e.projects[munged]
	e.mu.RUnlock()
	if !ok {
		return
	}
	st.mu.Lock()
	srv := st.streamer
	st.streamer = nil
	st.p.Streaming = false
	st.mu.Unlock()
	if srv != nil {
		srv.Flush(ctx)
		srv.Stop()
	}
}

// SFTP exposes the shared sftp client to other components (sendaway/bringback).
func (e *Engine) SFTP(ctx context.Context) (*sftp.Client, error) {
	return e.ssh.SFTP(ctx)
}

// Enqueue lets external callers (RPC handlers) trigger a refresh for a project.
func (e *Engine) Enqueue(munged string) {
	select {
	case e.dispatch <- event{kind: "discovered", munged: munged}:
	default:
	}
}

// RegisterProject installs (or refreshes) authoritative project data, used by
// send-away when the supplied cwd is more reliable than what discovery could
// reconstruct from the munged name (the '/'/'.' → '-' munge is lossy).
func (e *Engine) RegisterProject(munged, cwd, branch string) {
	e.updateProject(munged, func(p *Project) {
		p.Path = cwd
		p.IsGit = true
		if branch != "" {
			p.ActiveBranch = branch
		}
		// Drop the "non-applicable" lie discovery may have written.
		if p.RemoteState == StateNotApplicable {
			p.RemoteState = StateClonePending
		}
	})
	e.persist(munged)
}

// BootstrapNow runs the bootstrap inline; used by send-away when the project
// has never been seen by the scheduler.
func (e *Engine) BootstrapNow(ctx context.Context, munged, cwd string) error {
	if err := e.bootstrap(ctx, munged, cwd); err != nil {
		e.updateProject(munged, func(p *Project) {
			p.RemoteState = StateCloneFailed
			p.LastError = err.Error()
		})
		e.persist(munged)
		return err
	}
	e.updateProject(munged, func(p *Project) {
		p.Path = cwd
		p.IsGit = true
		p.RemoteState = StateSynced
		p.LastMirrorPush = time.Now()
		p.LastError = ""
	})
	e.persist(munged)
	return nil
}

// MirrorPushNow is the inline catch-up push send-away invokes before the
// worktree reconcile.
func (e *Engine) MirrorPushNow(ctx context.Context, munged, cwd string) error {
	if err := e.mirrorPush(ctx, munged, cwd); err != nil {
		e.updateProject(munged, func(p *Project) { p.LastError = err.Error() })
		e.persist(munged)
		return err
	}
	e.updateProject(munged, func(p *Project) {
		p.LastMirrorPush = time.Now()
		p.LastError = ""
	})
	e.persist(munged)
	return nil
}

// UploadAllSessionsNow ships every local .jsonl to the remote, used for inline
// bootstrap and overflow recovery.
func (e *Engine) UploadAllSessionsNow(ctx context.Context, munged string) error {
	files, err := e.SessionFilesFor(munged)
	if err != nil {
		return err
	}
	cli, err := e.ssh.SFTP(ctx)
	if err != nil {
		return err
	}
	remoteDir := filepath.Join(e.ssh.RemoteHome(), ".claude", "projects", munged)
	if err := mkdirAllSFTP(cli, remoteDir); err != nil {
		return err
	}
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if err := uploadFileSFTP(cli, f, filepath.Join(remoteDir, id+".jsonl")); err != nil {
			return err
		}
	}
	return nil
}

// DownloadAllSessions pulls every .jsonl from the remote project dir to local,
// overwriting matching ids. Used by bring-back.
func (e *Engine) DownloadAllSessions(ctx context.Context, munged string) (int, error) {
	cli, err := e.ssh.SFTP(ctx)
	if err != nil {
		return 0, err
	}
	remoteDir := filepath.Join(e.ssh.RemoteHome(), ".claude", "projects", munged)
	localDir := e.SessionDir(munged)
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		return 0, err
	}
	entries, err := cli.ReadDir(remoteDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, en := range entries {
		if en.IsDir() {
			continue
		}
		if !strings.HasSuffix(en.Name(), ".jsonl") {
			continue
		}
		if err := downloadFileSFTP(cli, filepath.Join(remoteDir, en.Name()), filepath.Join(localDir, en.Name())); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
