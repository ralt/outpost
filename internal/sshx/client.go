// Package sshx is the single in-process SSH client that every remote
// operation routes through. See spec 05.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ralt/outpost/internal/config"
	"github.com/ralt/outpost/internal/logging"
)

// State is a snapshot of the SSH layer for the status RPC.
type State struct {
	Connected bool
	Host      string
	User      string
	Home      string
	LastError string
}

// Client owns the persistent ssh.Client and the reconnect/keepalive goroutine.
type Client struct {
	cfg config.Remote
	log *slog.Logger

	hostUser string
	hostName string
	addr     string
	localHome string

	mu       sync.Mutex
	cond     *sync.Cond
	ssh      *ssh.Client
	sftp     *sftp.Client
	remoteHome string
	connected bool
	lastErr   error

	stopCh chan struct{}
	stopped bool
}

// New constructs the client but does not dial. Run Start to bring it up.
func New(c config.Config, log *slog.Logger) *Client {
	cli := &Client{
		cfg:      c.Remote,
		log:      logging.WithComponent(log, logging.CompSSH),
		hostUser: c.HostUser(),
		hostName: c.HostName(),
		addr:     net.JoinHostPort(c.HostName(), portString(c.Remote.Port)),
		localHome: os.Getenv("HOME"),
		stopCh:   make(chan struct{}),
	}
	cli.cond = sync.NewCond(&cli.mu)
	return cli
}

// Start kicks off the connect+keepalive loop in the background. It returns
// immediately. If remote.host is empty Start is a no-op and Snapshot reports
// the disabled state.
func (c *Client) Start(ctx context.Context) {
	if c.hostName == "" {
		c.setError(errors.New("no remote host configured"))
		return
	}
	go c.loop(ctx)
}

// Stop tears down the connection and joins the loop.
func (c *Client) Stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	close(c.stopCh)
	if c.sftp != nil {
		_ = c.sftp.Close()
		c.sftp = nil
	}
	if c.ssh != nil {
		_ = c.ssh.Close()
		c.ssh = nil
	}
	c.connected = false
	c.cond.Broadcast()
	c.mu.Unlock()
}

// Snapshot returns the public state for the status RPC.
func (c *Client) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := State{
		Connected: c.connected,
		Host:      c.cfg.Host,
		User:      c.hostUser,
		Home:      c.remoteHome,
	}
	if c.lastErr != nil {
		st.LastError = c.lastErr.Error()
	}
	return st
}

// Wait blocks until either a usable connection is available (returns the
// ssh.Client) or the ctx is cancelled. Used by the foreground RPCs that need
// the client right now.
func (c *Client) Wait(ctx context.Context) (*ssh.Client, error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.cond.L.Lock()
			c.cond.Broadcast()
			c.cond.L.Unlock()
		case <-done:
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	for !c.connected && !c.stopped && ctx.Err() == nil {
		c.cond.Wait()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if c.stopped {
		return nil, errors.New("ssh: client stopped")
	}
	return c.ssh, nil
}

// HostUser returns user@host string for logging.
func (c *Client) HostUser() string {
	if c.hostUser == "" {
		return c.hostName
	}
	return c.hostUser + "@" + c.hostName
}

// RemoteHome returns the cached remote $HOME or empty if not connected.
func (c *Client) RemoteHome() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remoteHome
}

// Run is a convenience: open a session, run cmd, return stdout/stderr.
func (c *Client) Run(ctx context.Context, cmd string) (stdout, stderr []byte, err error) {
	cli, err := c.Wait(ctx)
	if err != nil {
		return nil, nil, err
	}
	sess, err := cli.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("ssh: new session: %w", err)
	}
	defer sess.Close()
	var so, se bytes.Buffer
	sess.Stdout = &so
	sess.Stderr = &se
	if err := sess.Run(cmd); err != nil {
		return so.Bytes(), se.Bytes(), err
	}
	return so.Bytes(), se.Bytes(), nil
}

// RunStdin runs cmd with stdin piped from r and returns stdout/stderr.
func (c *Client) RunStdin(ctx context.Context, cmd string, r io.Reader) (stdout, stderr []byte, err error) {
	cli, err := c.Wait(ctx)
	if err != nil {
		return nil, nil, err
	}
	sess, err := cli.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("ssh: new session: %w", err)
	}
	defer sess.Close()
	var so, se bytes.Buffer
	sess.Stdout = &so
	sess.Stderr = &se
	sess.Stdin = r
	if err := sess.Run(cmd); err != nil {
		return so.Bytes(), se.Bytes(), err
	}
	return so.Bytes(), se.Bytes(), nil
}

// RunStream opens a session for cmd and streams stdout into w. Stderr is
// captured and returned. Use this when stdout is large (e.g. tar streams).
func (c *Client) RunStream(ctx context.Context, cmd string, w io.Writer) (stderr []byte, err error) {
	cli, err := c.Wait(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := cli.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh: new session: %w", err)
	}
	defer sess.Close()
	var se bytes.Buffer
	sess.Stdout = w
	sess.Stderr = &se
	if err := sess.Run(cmd); err != nil {
		return se.Bytes(), err
	}
	return se.Bytes(), nil
}

// SFTP returns the cached sftp.Client over the same ssh.Client, reconnecting
// it transparently on reconnects.
func (c *Client) SFTP(ctx context.Context) (*sftp.Client, error) {
	if _, err := c.Wait(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sftp != nil {
		return c.sftp, nil
	}
	cli := c.ssh
	if cli == nil {
		return nil, errors.New("ssh: client not connected")
	}
	s, err := sftp.NewClient(cli)
	if err != nil {
		return nil, fmt.Errorf("ssh: sftp: %w", err)
	}
	c.sftp = s
	return s, nil
}

// SSH returns the live underlying *ssh.Client, blocking until ready.
func (c *Client) SSH(ctx context.Context) (*ssh.Client, error) {
	return c.Wait(ctx)
}

// ── connect / reconnect loop ───────────────────────────────────────

func (c *Client) loop(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		default:
		}
		if err := c.dial(ctx); err != nil {
			c.setError(err)
			c.log.Warn("ssh dial failed", "err", err, "next", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			case <-c.stopCh:
				return
			}
			if backoff < 60*time.Second {
				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			}
			continue
		}
		backoff = 1 * time.Second
		c.runKeepalive(ctx)
		// Connection dropped — close + retry.
		c.disconnect("connection lost")
	}
}

func (c *Client) dial(ctx context.Context) error {
	signers, err := c.authSigners()
	if err != nil {
		return err
	}
	if len(signers) == 0 {
		return errors.New("AUTH_NO_KEYS: no usable identity (set remote.identity_file, add a key to ssh-agent, or place one at ~/.ssh/id_ed25519)")
	}

	hostKeyCB, hostKeyAlgos, err := c.hostKeyCallback()
	if err != nil {
		return err
	}

	user := c.hostUser
	if user == "" {
		user = os.Getenv("USER")
	}

	conf := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signers...),
		},
		HostKeyCallback: hostKeyCB,
		HostKeyAlgorithms: hostKeyAlgos,
		Timeout: 20 * time.Second,
	}

	d := net.Dialer{Timeout: 20 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, c.addr, conf)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("handshake: %w", err)
	}
	cli := ssh.NewClient(sshConn, chans, reqs)

	// $HOME check.
	remoteHome, err := runOne(cli, "printf '%s' \"$HOME\"")
	if err != nil {
		_ = cli.Close()
		return fmt.Errorf("home probe: %w", err)
	}
	remoteHome = strings.TrimSpace(remoteHome)
	if remoteHome == "" {
		_ = cli.Close()
		return fmt.Errorf("HOME_MISMATCH: remote $HOME unreadable")
	}
	if c.localHome != "" && remoteHome != c.localHome {
		_ = cli.Close()
		return fmt.Errorf("HOME_MISMATCH: local=%s remote=%s", c.localHome, remoteHome)
	}

	c.mu.Lock()
	c.ssh = cli
	c.remoteHome = remoteHome
	c.connected = true
	c.lastErr = nil
	if c.sftp != nil {
		_ = c.sftp.Close()
		c.sftp = nil
	}
	c.cond.Broadcast()
	c.mu.Unlock()
	c.log.Info("ssh connected", "host", c.HostUser(), "home", remoteHome)
	return nil
}

func runOne(cli *ssh.Client, cmd string) (string, error) {
	sess, err := cli.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.Run(cmd); err != nil {
		return "", err
	}
	return out.String(), nil
}

func (c *Client) runKeepalive(ctx context.Context) {
	interval := c.cfg.KeepaliveInterval
	if interval <= 0 {
		// Pure block until connection drops or shutdown.
		<-c.deadOrDone(ctx)
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-t.C:
			c.mu.Lock()
			cli := c.ssh
			c.mu.Unlock()
			if cli == nil {
				return
			}
			_, _, err := cli.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				c.log.Warn("ssh keepalive failed", "err", err)
				return
			}
		}
	}
}

// deadOrDone returns a channel that closes when the ssh.Client's underlying
// connection drops or when stop is signalled.
func (c *Client) deadOrDone(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		c.mu.Lock()
		cli := c.ssh
		c.mu.Unlock()
		if cli == nil {
			return
		}
		errCh := make(chan error, 1)
		go func() { errCh <- cli.Wait() }()
		select {
		case <-errCh:
		case <-ctx.Done():
		case <-c.stopCh:
		}
	}()
	return ch
}

func (c *Client) disconnect(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sftp != nil {
		_ = c.sftp.Close()
		c.sftp = nil
	}
	if c.ssh != nil {
		_ = c.ssh.Close()
		c.ssh = nil
	}
	c.connected = false
	c.log.Warn("ssh disconnected", "reason", reason)
}

func (c *Client) setError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	c.lastErr = err
}

// ── auth + host keys ──────────────────────────────────────────────

func (c *Client) authSigners() ([]ssh.Signer, error) {
	var signers []ssh.Signer
	if c.cfg.IdentityFile != "" {
		s, err := loadIdentityFile(c.cfg.IdentityFile)
		if err != nil {
			return nil, fmt.Errorf("load identity %s: %w", c.cfg.IdentityFile, err)
		}
		signers = append(signers, s)
	}
	// ssh-agent
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			if sigs, err := ag.Signers(); err == nil {
				signers = append(signers, sigs...)
			}
		}
	}
	// default key files
	home, _ := os.UserHomeDir()
	for _, name := range []string{"id_ed25519", "id_rsa"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if s, err := loadIdentityFile(p); err == nil {
			signers = append(signers, s)
		}
	}
	return signers, nil
}

func loadIdentityFile(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

func (c *Client) hostKeyCallback() (ssh.HostKeyCallback, []string, error) {
	khPath := c.cfg.KnownHostsFile
	if khPath == "" {
		home, _ := os.UserHomeDir()
		khPath = filepath.Join(home, ".ssh", "known_hosts")
	}
	if err := os.MkdirAll(filepath.Dir(khPath), 0o700); err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(khPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			f, err := os.OpenFile(khPath, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, nil, fmt.Errorf("create known_hosts: %w", err)
			}
			_ = f.Close()
		} else {
			return nil, nil, err
		}
	}
	kh, err := knownhosts.New(khPath)
	if err != nil {
		return nil, nil, fmt.Errorf("known_hosts: %w", err)
	}
	cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := kh(hostname, remote, key)
		if err == nil {
			return nil
		}
		var kerr *knownhosts.KeyError
		if errors.As(err, &kerr) {
			if len(kerr.Want) == 0 {
				// Unknown host — pin.
				if perr := pinHostKey(khPath, hostname, remote, key); perr != nil {
					return fmt.Errorf("pin host key: %w", perr)
				}
				fp := ssh.FingerprintSHA256(key)
				c.log.Info("host-key-pinned", "host", hostname, "fingerprint", fp)
				return nil
			}
			return fmt.Errorf("KNOWN_HOSTS_MISMATCH: %s key changed (got %s)", hostname, ssh.FingerprintSHA256(key))
		}
		return err
	}
	return cb, nil, nil
}

func pinHostKey(khPath, hostname string, remote net.Addr, key ssh.PublicKey) error {
	addrs := []string{}
	if remote != nil {
		addrs = append(addrs, knownhosts.Normalize(remote.String()))
	}
	addrs = append(addrs, knownhosts.Normalize(hostname))
	line := knownhosts.Line(addrs, key)
	f, err := os.OpenFile(khPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return nil
}

func portString(p int) string {
	if p == 0 {
		return "22"
	}
	return fmt.Sprintf("%d", p)
}
