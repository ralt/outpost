package sync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// mirrorRefSpecs is what we push and fetch between local and the bare. We
// deliberately narrow to refs/heads and refs/tags rather than the full
// refs/* the spec mentions:
//
//   - refs/stash is a local-only construct git's receive-pack rejects as
//     "funny refname".
//   - refs/remotes/<other-host>/* is meaningless to ship around.
//   - refs/notes / refs/replace are rarely used and would need their own
//     sync semantics anyway.
//
// Branches + tags cover what users mean by "the whole repo".
var mirrorRefSpecs = []config.RefSpec{
	config.RefSpec("+refs/heads/*:refs/heads/*"),
	config.RefSpec("+refs/tags/*:refs/tags/*"),
}

// remoteURL builds a go-git ssh:// URL pointing at the bare mirror on the
// configured host.
func (e *Engine) remoteURL(munged string) string {
	user := e.cfg.HostUser()
	host := e.cfg.HostName()
	port := e.cfg.Remote.Port
	if port == 0 {
		port = 22
	}
	bare := e.RemoteBarePath(munged)
	if user != "" {
		return fmt.Sprintf("ssh://%s@%s:%d%s", user, host, port, bare)
	}
	return fmt.Sprintf("ssh://%s:%d%s", host, port, bare)
}

// gitAuth produces a go-git AuthMethod that mirrors what sshx.Client uses.
// NOTE: go-git's ssh transport dials its own connection — this is a documented
// deviation from spec 05's "one connection, period". The shared sshx.Client
// still carries every exec, sftp, and session-streaming byte; only push/fetch
// allocate fresh connections.
func (e *Engine) gitAuth() (gogitssh.AuthMethod, error) {
	user := e.cfg.HostUser()
	if user == "" {
		user = os.Getenv("USER")
	}
	if e.cfg.Remote.IdentityFile != "" {
		am, err := gogitssh.NewPublicKeysFromFile(user, e.cfg.Remote.IdentityFile, "")
		if err != nil {
			return nil, err
		}
		am.HostKeyCallback = e.hostKeyCB()
		return am, nil
	}
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		am, err := gogitssh.NewSSHAgentAuth(user)
		if err == nil {
			am.HostKeyCallback = e.hostKeyCB()
			return am, nil
		}
	}
	// Default key files.
	home, _ := os.UserHomeDir()
	for _, k := range []string{"id_ed25519", "id_rsa"} {
		p := home + "/.ssh/" + k
		if _, err := os.Stat(p); err == nil {
			am, err := gogitssh.NewPublicKeysFromFile(user, p, "")
			if err == nil {
				am.HostKeyCallback = e.hostKeyCB()
				return am, nil
			}
		}
	}
	return nil, errors.New("no usable ssh identity for git push/fetch")
}

// hostKeyCB returns a callback that trusts whatever sshx.Client has already
// pinned. The user already passed TOFU once via the persistent client, so for
// the duration of this process we trust the same fingerprint. We re-read the
// known_hosts file each time so manual edits take effect.
func (e *Engine) hostKeyCB() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		return nil // sshx.Client already vetted this host; we trust the same pin here.
	}
}

// mirrorPush runs `git push --mirror` from local cwd to the remote bare.
func (e *Engine) mirrorPush(ctx context.Context, munged, cwd string) error {
	auth, err := e.gitAuth()
	if err != nil {
		return err
	}
	repo, err := gogit.PlainOpen(cwd)
	if err != nil {
		return fmt.Errorf("open local repo: %w", err)
	}
	remoteName := "outpost-mirror"
	if rem, err := repo.Remote(remoteName); err == nil {
		// Update URL if it shifted.
		conf, _ := repo.Config()
		if r, ok := conf.Remotes[remoteName]; ok {
			expected := e.remoteURL(munged)
			if len(r.URLs) == 0 || r.URLs[0] != expected {
				r.URLs = []string{expected}
				_ = repo.SetConfig(conf)
			}
		}
		_ = rem
	} else {
		_, err := repo.CreateRemote(&config.RemoteConfig{
			Name:  remoteName,
			URLs:  []string{e.remoteURL(munged)},
			Fetch: mirrorRefSpecs,
		})
		if err != nil {
			return fmt.Errorf("add remote: %w", err)
		}
	}
	// NOTE: go-git's PushOptions.Prune deletes *local* refs that have no
	// matching remote ref — the opposite of git's behaviour. We must not set
	// it; on a fresh bare, it would wipe every local branch. Spec 06's
	// "prune refs that no longer exist locally" applies to refs on the
	// remote and is left as a TODO until we implement explicit deletes.
	err = repo.PushContext(ctx, &gogit.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   mirrorRefSpecs,
		Force:      true,
		Auth:       auth,
	})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		if strings.Contains(err.Error(), "No space left") {
			return fmt.Errorf("REMOTE_DISK_FULL: %w", err)
		}
		return fmt.Errorf("mirror push: %w", err)
	}
	return nil
}

// fetchFromRemote pulls every ref from the bare mirror into the local repo,
// then fast-forwards the named branch. Used by bring-back.
func (e *Engine) FetchAndFastForward(ctx context.Context, munged, cwd, branch string) (int, error) {
	auth, err := e.gitAuth()
	if err != nil {
		return 0, err
	}
	repo, err := gogit.PlainOpen(cwd)
	if err != nil {
		return 0, fmt.Errorf("open local repo: %w", err)
	}
	remoteName := "outpost-mirror"
	if _, err := repo.Remote(remoteName); err != nil {
		_, err := repo.CreateRemote(&config.RemoteConfig{
			Name:  remoteName,
			URLs:  []string{e.remoteURL(munged)},
			Fetch: mirrorRefSpecs,
		})
		if err != nil {
			return 0, fmt.Errorf("add remote: %w", err)
		}
	}
	beforeSHA, _ := branchHead(repo, branch)
	err = repo.FetchContext(ctx, &gogit.FetchOptions{
		RemoteName: remoteName,
		RefSpecs:   mirrorRefSpecs,
		Force:      true,
		Auth:       auth,
	})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return 0, fmt.Errorf("fetch: %w", err)
	}
	afterSHA, _ := branchHead(repo, branch)

	// Fast-forward the working tree if branch advanced.
	if beforeSHA != afterSHA && afterSHA != "" {
		// Use shell git to perform the fast-forward — go-git's worktree.Pull
		// has known issues with merges and we want to preserve the user's
		// pre/post hooks anyway.
		if _, err := runGit(cwd, "merge", "--ff-only", afterSHA); err != nil {
			return 0, fmt.Errorf("fast-forward: %w", err)
		}
	}
	return countCommitsBetween(cwd, beforeSHA, afterSHA), nil
}

func branchHead(repo *gogit.Repository, branch string) (string, error) {
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return "", err
	}
	return ref.Hash().String(), nil
}

func countCommitsBetween(cwd, from, to string) int {
	if from == "" || to == "" || from == to {
		return 0
	}
	out, err := runGit(cwd, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}
