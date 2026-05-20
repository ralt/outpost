package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/ralt/outpost/internal/config"
	"github.com/ralt/outpost/internal/ctl"
	"github.com/ralt/outpost/internal/daemon"
	"github.com/ralt/outpost/internal/logging"
	"github.com/ralt/outpost/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "daemon":
		os.Exit(cmdDaemon(args))
	case "send-away":
		os.Exit(cmdSimple("send-away", args))
	case "bring-back":
		os.Exit(cmdBringBack(args))
	case "status":
		os.Exit(cmdSimple("status", args))
	case "projects":
		os.Exit(cmdSimple("projects", args))
	case "reload":
		os.Exit(cmdSimple("reload", args))
	case "stop":
		os.Exit(cmdStop(args))
	case "logs":
		os.Exit(cmdLogs(args))
	case "config-path":
		os.Exit(cmdConfigPath(args))
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		os.Exit(0)
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: outpost <command> [args]

Daemon:
  daemon              Run the long-lived daemon (FUSE mount + SSH + sync).
  stop                Send a clean-shutdown RPC to a running daemon.

Client commands (talk to the running daemon):
  send-away [--json]  Push the current project to the remote and launch headless.
  bring-back [--yes] [--json]  Pull remote progress back and apply locally.
  status [--json]     Print daemon + remote status.
  projects [--json]   List every project the daemon knows about.
  reload              Reload the daemon's config.

Utilities:
  logs [--since 10m] [--req <id>]  Tail journald (or print a hint if not under systemd).
  config-path         Print which config file the daemon would load.
  version             Print the build version.
  help                Print this text.`)
}

// ── subcommands ─────────────────────────────────────────────────────

func cmdDaemon(args []string) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	log := logging.New(logging.Options{Level: cfg.Logging.Level, Format: cfg.Logging.Format})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := daemon.Run(ctx, cfg, log); err != nil {
		fmt.Fprintln(os.Stderr, "daemon:", err)
		return 1
	}
	return 0
}

func cmdSimple(method string, args []string) int {
	jsonOut := hasFlag(args, "--json")
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	resp, err := call(cfg.Paths.ControlSocket, method, currentSendAwayArgs(method))
	if err != nil {
		if ctl.IsNotRunning(err) {
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintln(os.Stderr, "Hint: systemctl --user start outpost.service")
			return 3
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return printResponse(method, resp, jsonOut)
}

func cmdBringBack(args []string) int {
	jsonOut := hasFlag(args, "--json")
	yes := hasFlag(args, "--yes") || hasFlag(args, "-y")
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	cwd, _ := os.Getwd()
	resp, err := call(cfg.Paths.ControlSocket, "bring-back", ctl.BringBackArgs{Cwd: cwd, Confirmed: yes})
	if err != nil {
		if ctl.IsNotRunning(err) {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// Two flows: needs-confirm flag set by daemon → prompt y/N (unless --yes).
	if !resp.OK && resp.Code == ctl.CodeNeedsConfirm && !yes {
		var data ctl.BringBackResult
		_ = json.Unmarshal(resp.Data, &data)
		if !isTTY(os.Stdin) {
			fmt.Fprintln(os.Stderr, resp.Error)
			fmt.Fprintln(os.Stderr, "Re-run with --yes to confirm.")
			return 1
		}
		fmt.Printf("bring-back will overwrite %d local session file(s) with the remote's copy.\n", data.WillOverwrite)
		fmt.Print("Proceed? [y/N] ")
		var ans string
		fmt.Fscanln(os.Stdin, &ans)
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
			fmt.Println("aborted.")
			return 1
		}
		// Second call with confirmed=true.
		resp, err = call(cfg.Paths.ControlSocket, "bring-back", ctl.BringBackArgs{Cwd: cwd, Confirmed: true})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	return printResponse("bring-back", resp, jsonOut)
}

func cmdStop(args []string) int {
	cfg, err := loadConfig()
	if err != nil {
		return 0
	}
	_, _ = call(cfg.Paths.ControlSocket, "shutdown", nil) // best-effort
	// Wait up to 5s for the socket to disappear.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.Paths.ControlSocket); errors.Is(err, os.ErrNotExist) {
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0
}

func cmdLogs(args []string) int {
	since := ""
	reqID := ""
	for i, a := range args {
		switch a {
		case "--since":
			if i+1 < len(args) {
				since = args[i+1]
			}
		case "--req":
			if i+1 < len(args) {
				reqID = args[i+1]
			}
		}
	}
	if _, err := exec.LookPath("journalctl"); err != nil {
		fmt.Fprintln(os.Stderr, "journalctl not found — tail the daemon's stderr directly (e.g. when running `outpost daemon` by hand).")
		return 0
	}
	jargs := []string{"--user", "-u", "outpost.service", "-f"}
	if since != "" {
		jargs = append(jargs, "--since", since)
	}
	if reqID != "" {
		jargs = append(jargs, "--grep", "req="+reqID)
	}
	c := exec.Command("journalctl", jargs...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	if err := c.Run(); err != nil {
		return 1
	}
	return 0
}

func cmdConfigPath(args []string) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if cfg.SourcePath == "" {
		fmt.Printf("(no config file; would load from %s)\n", config.DefaultPath())
	} else {
		fmt.Println(cfg.SourcePath)
	}
	return 0
}

// ── helpers ─────────────────────────────────────────────────────────

func loadConfig() (config.Config, error) {
	return config.Load("")
}

func call(socket, method string, args any) (*ctl.Response, error) {
	cli := ctl.NewClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return cli.Call(ctx, method, args)
}

// currentSendAwayArgs builds the right args struct for the method when it
// implicitly needs cwd.
func currentSendAwayArgs(method string) any {
	switch method {
	case "send-away":
		cwd, _ := os.Getwd()
		return ctl.SendAwayArgs{Cwd: cwd}
	default:
		return nil
	}
}

func printResponse(method string, resp *ctl.Response, jsonOut bool) int {
	if jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		if !resp.OK {
			return 1
		}
		return 0
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", resp.Code, resp.Error)
		if resp.Req != "" {
			fmt.Fprintf(os.Stderr, "trace id: %s (journalctl --user -u outpost.service --grep req=%s)\n", resp.Req, resp.Req)
		}
		return 1
	}
	switch method {
	case "status":
		var s ctl.StatusResult
		_ = json.Unmarshal(resp.Data, &s)
		fmt.Printf("mount      : %s (%s)\n", s.Mount.Path, mountedStr(s.Mount.Mounted))
		fmt.Printf("backing    : %s\n", s.Backing)
		fmt.Printf("remote     : %s (%s)\n", s.Remote.Host, connectedStr(s.Remote.Connected))
		if s.Remote.Home != "" {
			fmt.Printf("remote home: %s\n", s.Remote.Home)
		}
		if s.Remote.LastError != "" {
			fmt.Printf("last error : %s\n", s.Remote.LastError)
		}
		fmt.Printf("projects   : %d\n", s.Projects)
		fmt.Printf("version    : %s\n", s.Version)
	case "projects":
		var pr ctl.ProjectsResult
		_ = json.Unmarshal(resp.Data, &pr)
		if len(pr.Projects) == 0 {
			fmt.Println("(no projects)")
			return 0
		}
		for _, p := range pr.Projects {
			git := "non-git"
			if p.IsGit {
				git = "git"
			}
			fmt.Printf("- %s  [%s]\n", p.Name, p.Owner)
			fmt.Printf("    path:       %s\n", p.Path)
			fmt.Printf("    %s         %d session(s)", git, p.Sessions)
			if p.LatestSession != "" {
				fmt.Printf("  latest=%s", p.LatestSession)
			}
			fmt.Println()
			fmt.Printf("    state:      %s  streaming=%v\n", p.RemoteState, p.Streaming)
		}
	case "send-away":
		var s ctl.SendAwayResult
		_ = json.Unmarshal(resp.Data, &s)
		fmt.Printf("✓ Project: %s\n", s.Project)
		if s.BootstrapState != "" {
			fmt.Printf("  (state: %s — first time for this project)\n", s.BootstrapState)
		}
		if s.Branch != "" {
			fmt.Printf("✓ Worktree reconciled to %s @ %s\n", s.Branch, s.Head)
		}
		fmt.Printf("✓ Uncommitted: %d hunk(s) / %d untracked file(s)\n", s.UncommittedHunks, s.Untracked)
		fmt.Printf("✓ %d agent(s):\n", len(s.Agents))
		for _, a := range s.Agents {
			fmt.Printf("    - %s → PID %d  (%s)\n", a.Session, a.PID, a.State)
		}
		fmt.Println("✓ Run `outpost bring-back` when you're back.")
	case "bring-back":
		var b ctl.BringBackResult
		_ = json.Unmarshal(resp.Data, &b)
		fmt.Printf("✓ Project: %s\n", b.Project)
		fmt.Printf("✓ %d commit(s) fast-forwarded\n", b.CommitsPulled)
		fmt.Printf("✓ %d hunk(s), %d untracked file(s) applied\n", b.Hunks, b.Untracked)
		fmt.Printf("✓ %d session file(s) pulled\n", b.Sessions)
	default:
		// reload / unknown - just dump.
		if len(resp.Data) > 0 {
			fmt.Println(string(resp.Data))
		} else {
			fmt.Println("ok")
		}
	}
	if resp.Req != "" {
		fmt.Printf("(trace id: %s)\n", resp.Req)
	}
	return 0
}

func mountedStr(b bool) string {
	if b {
		return "mounted"
	}
	return "not mounted"
}

func connectedStr(b bool) string {
	if b {
		return "connected"
	}
	return "disconnected"
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// stubbed; lets `outpost stop` always discover a sane errno value to ignore.
var _ = syscall.ENOENT
