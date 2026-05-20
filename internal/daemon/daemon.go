// Package daemon wires every component together for the long-running
// `outpost daemon` process.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ralt/outpost/internal/assets"
	"github.com/ralt/outpost/internal/bringback"
	"github.com/ralt/outpost/internal/config"
	"github.com/ralt/outpost/internal/ctl"
	"github.com/ralt/outpost/internal/fusefs"
	"github.com/ralt/outpost/internal/logging"
	"github.com/ralt/outpost/internal/sendaway"
	"github.com/ralt/outpost/internal/sshx"
	syncpkg "github.com/ralt/outpost/internal/sync"
	"github.com/ralt/outpost/internal/version"
)

// Run boots the daemon and blocks until ctx is cancelled or a fatal error
// surfaces.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	rootLog := logging.WithComponent(log, logging.CompDaemon)
	rootLog.Info("starting", "version", version.Version, "config", cfg.SourcePath)

	// FUSE mount.
	fs := fusefs.New(cfg.Paths.Backing, cfg.Paths.Mount, assets.Commands(), log)
	unmount, err := fs.Mount()
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	defer func() {
		if err := unmount(); err != nil {
			rootLog.Warn("unmount", "err", err)
		}
	}()

	// SSH client.
	ssh := sshx.New(cfg, log)
	ssh.Start(ctx)
	defer ssh.Stop()

	// Sync engine.
	eng := syncpkg.New(cfg, ssh, log)
	fs.SetSink(eng)
	if err := eng.Start(ctx); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	defer eng.Stop()

	// Send-away / bring-back pipelines.
	sa := sendaway.New(cfg, ssh, eng, log)
	bb := bringback.New(cfg, ssh, eng, log)

	// Control socket.
	handler := &rpcHandler{
		cfg:  cfg,
		log:  rootLog,
		fs:   fs,
		ssh:  ssh,
		eng:  eng,
		sa:   sa,
		bb:   bb,
		reload: func() error { return errors.New("reload: not implemented") },
	}
	srv := ctl.NewServer(cfg.Paths.ControlSocket, handler, log)
	if err := srv.Listen(); err != nil {
		return err
	}
	defer srv.Close()

	// systemd notify (best-effort; no-op if not under systemd).
	notifyReady()

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	shutdownCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case s := <-sigCh:
			rootLog.Info("signal received, shutting down", "signal", s.String())
			cancel()
		case <-shutdownCtx.Done():
		}
	}()

	rootLog.Info("ready")
	err = srv.Serve(shutdownCtx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	rootLog.Info("stopped")
	return nil
}

// rpcHandler implements ctl.Handler by dispatching to the right component.
type rpcHandler struct {
	cfg config.Config
	log *slog.Logger
	fs  *fusefs.FS
	ssh *sshx.Client
	eng *syncpkg.Engine
	sa  *sendaway.Pipeline
	bb  *bringback.Pipeline

	reload func() error
}

func (h *rpcHandler) HandleSendAway(ctx context.Context, args ctl.SendAwayArgs) (ctl.SendAwayResult, error) {
	return h.sa.Run(ctx, args)
}

func (h *rpcHandler) HandleBringBack(ctx context.Context, args ctl.BringBackArgs) (ctl.BringBackResult, error) {
	return h.bb.Run(ctx, args)
}

func (h *rpcHandler) HandleStatus(ctx context.Context) (ctl.StatusResult, error) {
	res := ctl.StatusResult{}
	res.Mount.Path = h.cfg.Paths.Mount
	res.Mount.Mounted = mountpointActive(h.cfg.Paths.Mount)
	res.Backing = h.cfg.Paths.Backing
	st := h.ssh.Snapshot()
	res.Remote.Connected = st.Connected
	res.Remote.Host = st.Host
	res.Remote.Home = st.Home
	res.Remote.LastError = st.LastError
	res.Projects = len(h.eng.Projects())
	res.Version = version.Version
	return res, nil
}

func (h *rpcHandler) HandleProjects(ctx context.Context) (ctl.ProjectsResult, error) {
	out := ctl.ProjectsResult{}
	for _, p := range h.eng.Projects() {
		latest, _ := h.eng.LatestSession(p.Munged)
		sessionFiles, _ := h.eng.SessionFilesFor(p.Munged)
		owner := p.Owner
		if owner == "" {
			owner = syncpkg.OwnerLocal
		}
		state := p.RemoteState
		if state == "" {
			state = syncpkg.StateClonePending
		}
		latestName := ""
		if latest != "" {
			latestName = latest + ".jsonl"
		}
		out.Projects = append(out.Projects, ctl.ProjectInfo{
			Name:          p.Munged,
			Path:          p.Path,
			IsGit:         p.IsGit,
			Sessions:      len(sessionFiles),
			LatestSession: latestName,
			Owner:         owner,
			RemoteState:   state,
			Streaming:     p.Streaming,
		})
	}
	return out, nil
}

func (h *rpcHandler) HandleReload(ctx context.Context) error {
	// v1: reload is a no-op stub; full reload requires teardown of ssh+sync
	// and is out of scope for the initial cut.
	h.log.Warn("reload requested but not implemented in this build")
	return nil
}

// mountpointActive returns whether path is a FUSE mountpoint. We probe by
// stat'ing path + path/.. and comparing st_dev — different device means a
// mount sits there.
func mountpointActive(path string) bool {
	var inner, outer syscall.Stat_t
	if err := syscall.Stat(path, &inner); err != nil {
		return false
	}
	if err := syscall.Stat(filepath.Dir(path), &outer); err != nil {
		return false
	}
	return inner.Dev != outer.Dev
}

// notifyReady sends sd_notify(READY=1) when launched under systemd Type=notify.
// No-op if NOTIFY_SOCKET isn't set.
func notifyReady() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	// Abstract namespace: '@' prefix → "\x00…" address.
	addr := sock
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}
	c, err := net.Dial("unixgram", addr)
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = c.Write([]byte("READY=1\nSTATUS=Outpost is up and serving\n"))
}
