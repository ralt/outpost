package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"

	"github.com/ralt/outpost/internal/fusefs"
)

const (
	streamerDebounce   = 100 * time.Millisecond
	streamerRingBytes  = 1 * 1024 * 1024 // 1 MB
)

// streamer keeps the remote ~/.claude/projects/<munged>/<id>.jsonl in sync with
// every write into the local backing file. One streamer per project.
type streamer struct {
	engine *Engine
	munged string
	log    *slog.Logger

	mu       sync.Mutex
	pending  map[string]*pendingFile // id → state
	timer    *time.Timer
	stopCh   chan struct{}
	stopped  bool
	overflow bool
}

type pendingFile struct {
	id      string
	minOff  int64
	bytesQd int
	dirty   bool
}

func newStreamer(e *Engine, munged string, log *slog.Logger) *streamer {
	return &streamer{
		engine:  e,
		munged:  munged,
		log:     log.With("component", "stream"),
		pending: map[string]*pendingFile{},
		stopCh:  make(chan struct{}),
	}
}

func (s *streamer) Start(ctx context.Context) {
	// Eager catch-up: every existing .jsonl gets flushed once on startup.
	go s.initialUpload(ctx)
}

func (s *streamer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	if s.timer != nil {
		s.timer.Stop()
	}
	close(s.stopCh)
}

// OnWrite is invoked from the FUSE write path. Must not block.
func (s *streamer) OnWrite(ev fusefs.WriteEvent) {
	if ev.Munged != s.munged {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	p, ok := s.pending[ev.ID]
	if !ok {
		p = &pendingFile{id: ev.ID, minOff: ev.Offset}
		s.pending[ev.ID] = p
	}
	if ev.Offset < p.minOff {
		p.minOff = ev.Offset
	}
	p.bytesQd += ev.Length
	p.dirty = true
	if p.bytesQd > streamerRingBytes {
		s.overflow = true
	}
	s.scheduleFlush()
}

// OnSession handles create/unlink.
func (s *streamer) OnSession(ev fusefs.SessionLifecycleEvent) {
	if ev.Munged != s.munged {
		return
	}
	if ev.Kind == "create" {
		s.mu.Lock()
		if _, ok := s.pending[ev.ID]; !ok {
			s.pending[ev.ID] = &pendingFile{id: ev.ID, dirty: true}
		}
		s.scheduleFlush()
		s.mu.Unlock()
		return
	}
	if ev.Kind == "unlink" {
		// Best-effort remote delete; runs synchronously off the FUSE thread.
		go s.deleteRemote(context.Background(), ev.ID)
	}
}

func (s *streamer) scheduleFlush() {
	if s.timer != nil {
		s.timer.Reset(streamerDebounce)
		return
	}
	s.timer = time.AfterFunc(streamerDebounce, func() {
		s.mu.Lock()
		s.timer = nil
		s.mu.Unlock()
		s.flushOnce(context.Background())
	})
}

// Flush blocks until the queue is empty (or ctx fires). Used by send-away
// before flipping owner → remote.
func (s *streamer) Flush(ctx context.Context) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !s.hasPending() {
			return
		}
		s.flushOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *streamer) hasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pending {
		if p.dirty {
			return true
		}
	}
	return false
}

func (s *streamer) flushOnce(ctx context.Context) {
	s.mu.Lock()
	dirties := make([]*pendingFile, 0, len(s.pending))
	for _, p := range s.pending {
		if !p.dirty {
			continue
		}
		dirties = append(dirties, p)
	}
	s.mu.Unlock()
	if len(dirties) == 0 {
		return
	}
	cli, err := s.engine.ssh.SFTP(ctx)
	if err != nil {
		s.log.Debug("stream sftp unavailable", "err", err)
		return
	}
	if err := s.ensureRemoteDir(cli); err != nil {
		s.log.Warn("stream remote dir", "err", err)
		return
	}
	for _, p := range dirties {
		if err := s.uploadOne(cli, p.id); err != nil {
			s.log.Warn("stream upload", "id", p.id, "err", err)
			continue
		}
		s.mu.Lock()
		if cur, ok := s.pending[p.id]; ok {
			cur.dirty = false
			cur.bytesQd = 0
		}
		s.mu.Unlock()
	}
}

func (s *streamer) initialUpload(ctx context.Context) {
	files, err := s.engine.SessionFilesFor(s.munged)
	if err != nil {
		return
	}
	cli, err := s.engine.ssh.SFTP(ctx)
	if err != nil {
		return
	}
	if err := s.ensureRemoteDir(cli); err != nil {
		s.log.Warn("stream initial mkdir", "err", err)
		return
	}
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if err := s.uploadOne(cli, id); err != nil {
			s.log.Warn("stream initial", "id", id, "err", err)
		}
	}
}

func (s *streamer) ensureRemoteDir(cli *sftp.Client) error {
	dir := s.remoteDir()
	return mkdirAllSFTP(cli, dir)
}

func (s *streamer) remoteDir() string {
	home := s.engine.ssh.RemoteHome()
	if home == "" {
		// fall back to assumption — should never be empty if SFTP() succeeded.
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "projects", s.munged)
}

func (s *streamer) uploadOne(cli *sftp.Client, id string) error {
	src := filepath.Join(s.engine.SessionDir(s.munged), id+".jsonl")
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.deleteRemoteWithCli(cli, id)
		}
		return err
	}
	defer in.Close()
	dst := filepath.Join(s.remoteDir(), id+".jsonl")
	out, err := cli.Create(dst)
	if err != nil {
		return fmt.Errorf("sftp create: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("sftp copy: %w", err)
	}
	return nil
}

func (s *streamer) deleteRemote(ctx context.Context, id string) {
	cli, err := s.engine.ssh.SFTP(ctx)
	if err != nil {
		return
	}
	_ = s.deleteRemoteWithCli(cli, id)
}

func (s *streamer) deleteRemoteWithCli(cli *sftp.Client, id string) error {
	dst := filepath.Join(s.remoteDir(), id+".jsonl")
	err := cli.Remove(dst)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// mkdirAllSFTP is sftp's missing recursive-mkdir.
func mkdirAllSFTP(cli *sftp.Client, dir string) error {
	dir = filepath.Clean(dir)
	if dir == "/" || dir == "." {
		return nil
	}
	if st, err := cli.Stat(dir); err == nil {
		if st.IsDir() {
			return nil
		}
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirAllSFTP(cli, parent); err != nil {
			return err
		}
	}
	if err := cli.Mkdir(dir); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return nil
}

