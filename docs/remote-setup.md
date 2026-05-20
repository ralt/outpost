# Setting up the remote host

Outpost talks to **one** remote machine over SSH. This doc lists everything
that has to be true on that machine before `outpost send-away` will work.

> **Just testing locally?** Skip ahead to
> [`scripts/dev-remote.sh`](../scripts/dev-remote.sh) — it spins up a Docker
> container that already satisfies every requirement below and rewrites your
> `config.ini` to point at it. `./scripts/dev-remote.sh up` and you're done.

## TL;DR

```bash
# On the remote, as the user outpost will SSH in as:
mkdir -p ~/.claude/projects ~/.local/share/outpost/{repos,logs,.meta}

# Make sure these are on PATH:
which git tar claude

# Pre-populate or generate keys on the local box, then:
ssh-copy-id user@remote.example.com

# Smoke:
ssh user@remote.example.com 'echo $HOME && git --version && claude --version'
```

If that last line prints the same `$HOME` as your local machine, plus
git and claude versions, you're done — fill in `[remote] host` in
`~/.config/outpost/config.ini` and run `outpost reload`.

The first time the daemon connects it pins the remote's host key into
`~/.ssh/known_hosts` (TOFU). Subsequent mismatches are hard-rejected.

## What has to be true

### 1. SSH access via key auth

- Public-key auth, no interactive password prompt.
- Passphrased keys are fine **only** if loaded into `ssh-agent` —
  the daemon never prompts.
- Either set `remote.identity_file` in `config.ini` or let outpost pick
  up `SSH_AUTH_SOCK` / `~/.ssh/id_ed25519` / `~/.ssh/id_rsa` in that
  order.

The daemon does **not** read `~/.ssh/config` (see [spec 05][05]). If
you rely on `ProxyJump`, host aliases, or non-default ports, put the
real host and port directly in `config.ini`:

```ini
[remote]
host = alice@dev-box.example.com
port = 2222
identity_file = /home/alice/.ssh/id_ed25519_outpost
```

### 2. Matching `$HOME`

Claude's session `.jsonl` files have absolute paths baked in — your
local cwd is encoded in the file name (`/foo/bar` →
`-foo-bar`) **and** in tool inputs/outputs inside the transcript. The
remote must therefore have a `$HOME` byte-identical to local, so the
reproduced cwd works.

In practice that means **same username, same OS family** — both Linux
with `/home/<user>`, or both macOS with `/Users/<user>`. Setups that
hit the same `$HOME` by other means (custom passwd entry, NFS-mounted
homes, bind mounts) also work; outpost checks the value, not the
provenance.

Verify:

```bash
ssh user@remote 'echo $HOME'         # → /home/alice
echo $HOME                           # → /home/alice   (must match)
```

If they differ the daemon refuses to operate; `outpost status` shows
`remote.last_error = HOME_MISMATCH: local=… remote=…`.

### 3. Required binaries on the remote `PATH`

The remote shells out via SSH for several operations:

| Binary    | Used for                                                  |
| --------- | --------------------------------------------------------- |
| `git`     | `init --bare`, `worktree add`, `switch`, `reset --hard`, `apply`, `diff`, `ls-files`, `status` |
| `tar`     | Receiving untracked files on send-away; sending them back on bring-back |
| `claude`  | The headless agent itself (`claude -p '…' --resume <id>`) |
| `sh`      | All exec commands are evaluated via the default login shell |
| `nohup`   | Detaches the headless agent so it survives SSH disconnect |
| `kill`    | Used by `bring-back` to terminate agents                  |

If `claude` lives under a non-standard path, set:

```ini
[remote]
claude_bin = /opt/anthropic/bin/claude
```

The daemon returns `REMOTE_MISSING_BIN` if `claude` isn't found.

### 4. Anthropic credentials on the remote

The remote runs Claude in headless mode (`claude -p '…' --resume <id>`),
which makes API calls just like your local Claude does. You need an
API key reachable from the remote's environment.

The headless process inherits the env of the login shell that runs
`nohup claude …` (we use `ssh.Session`, which by default sources the
remote `.bashrc`/`.zshrc` for interactive-style login). The easiest
setup is:

```bash
# In the remote user's ~/.bashrc (or equivalent):
export ANTHROPIC_API_KEY=sk-ant-…
```

Verify:

```bash
ssh user@remote 'claude -p "say hi" --max-turns 1'
```

If that returns a normal Claude response, the remote agent will work.

### 5. Disk space and writable paths

The daemon creates and writes under:

```
~/.local/share/outpost/repos/<munged>.git/            ← bare mirrors (one per project)
~/.local/share/outpost/logs/<munged>/<session>.log    ← per-agent stdout/stderr
~/.local/share/outpost/.meta/<munged>.json            ← per-project metadata
~/.claude/projects/<munged>/<session>.jsonl           ← session transcripts (streamed)
<every local cwd of a synced project>                 ← reproduced working trees
```

These are all under the remote user's `$HOME` and don't need root.
Size depends on project: each project costs roughly **one bare git
mirror + one full checkout**. Logs and `.jsonl` files grow over time.

### 6. Firewall / connectivity

Outpost makes exactly one TCP connection to `host:port` (default `22`)
and keeps it open. It sends `keepalive@openssh.com` global requests at
`remote.keepalive_interval` (default 30s). If the remote silently
drops idle SSH, raise the interval or set `ClientAliveInterval` on the
remote sshd.

## Step-by-step setup

These are the steps for a typical Linux-to-Linux setup. Adapt as
needed for macOS.

### On your local machine

1. Generate a key (if you don't already have one):
   ```bash
   ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519_outpost -C outpost
   ```

2. Install `outpost` and let `scripts/install.sh` write the default
   config:
   ```bash
   bash scripts/install.sh
   ```

3. Edit `~/.config/outpost/config.ini`:
   ```ini
   [remote]
   host = alice@dev-box.example.com
   port = 22
   identity_file = /home/alice/.ssh/id_ed25519_outpost
   ```

4. Don't restart the daemon yet — set up the remote first.

### On the remote

5. Copy the public key over:
   ```bash
   ssh-copy-id -i ~/.ssh/id_ed25519_outpost.pub alice@dev-box.example.com
   ```

6. SSH in once interactively to confirm it works:
   ```bash
   ssh -i ~/.ssh/id_ed25519_outpost alice@dev-box.example.com
   ```

7. On the remote, install prerequisites:
   ```bash
   # Debian/Ubuntu
   sudo apt-get install -y git tar
   # Fedora/RHEL
   sudo dnf install -y git tar
   # macOS (with Homebrew)
   brew install git
   ```

8. Install Claude Code on the remote following the standard
   instructions, and make sure `claude --version` works:
   ```bash
   claude --version
   ```

9. Export `ANTHROPIC_API_KEY` in the remote shell's startup file
   (`~/.bashrc`, `~/.zshrc`, or `~/.profile`). Verify in a fresh
   non-interactive shell, since that's the shape SSH commands run in:
   ```bash
   ssh alice@dev-box.example.com 'echo $ANTHROPIC_API_KEY | head -c 8'
   ```
   If that prints the prefix of your key, you're good. If it prints
   nothing, move the export to a file that non-interactive shells
   source (e.g. `~/.bash_env` with `BASH_ENV` exported on the
   *remote*, or `~/.zshenv` for zsh).

10. Pre-create the data dirs (optional — outpost will mkdir them on
    first use, but doing it explicitly lets you verify perms):
    ```bash
    mkdir -p ~/.claude/projects \
             ~/.local/share/outpost/{repos,logs,.meta}
    ```

### Back on the local machine

11. Confirm everything works without outpost first:
    ```bash
    ssh alice@dev-box.example.com 'echo $HOME && git --version && claude --version'
    ```
    Both versions should print and the `$HOME` should match
    `echo $HOME` on your local box.

12. Tell the running daemon to pick up the config:
    ```bash
    outpost reload     # or: systemctl --user restart outpost.service
    outpost status
    ```
    `remote.connected` should be `true`. If not, see the
    troubleshooting table below.

## Troubleshooting

The daemon surfaces errors via `outpost status` (`remote.last_error`)
and via the `code` field on `send-away` / `bring-back` RPC responses.

| Code / symptom              | What it means                                                                 | Fix                                                                                                |
| --------------------------- | ----------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `AUTH_NO_KEYS`              | The daemon found no usable identity (no `identity_file`, no agent, no defaults). | Set `remote.identity_file`, or `ssh-add` a key, or place one at `~/.ssh/id_ed25519`.            |
| `HOME_MISMATCH`             | `echo $HOME` over SSH didn't match local `$HOME`.                              | Use the same username on both ends, or arrange `$HOME` paths to match exactly.                     |
| `KNOWN_HOSTS_MISMATCH`      | The pinned host key changed since first connect.                              | Possible MITM — investigate. To re-pin: `ssh-keygen -R dev-box.example.com` then `outpost reload`. |
| `REMOTE_MISSING_BIN`        | `claude` is not on the remote `PATH`, or is at a non-default path.            | Install Claude on the remote, or set `remote.claude_bin = /full/path/to/claude`.                   |
| `NO_GIT`                    | Local cwd is not a git repo.                                                  | `git init` and at least one commit.                                                                |
| `NO_COMMITS`                | Branch has zero commits.                                                      | Make at least one commit.                                                                          |
| `REMOTE_DISK_FULL`          | Push failed due to remote ENOSPC.                                             | Free space under the remote `$HOME`.                                                                |
| `REMOTE_REJECT`             | Remote git/ssh rejected the push or reconcile.                                | Read the included stderr; usually a permission or dirty-worktree issue.                            |
| `DISCONNECTED`              | SSH is down at the moment of the RPC.                                         | `outpost status` shows the underlying reason; daemon auto-reconnects.                              |
| `ALREADY_SENT_AWAY`         | Project's owner is already `remote`.                                          | Run `outpost bring-back` first, or edit `~/.local/share/outpost/.meta/<munged>.json` to forfeit.    |
| `NOT_SENT_AWAY`             | bring-back called on a project with `owner=local`.                            | Nothing to bring back; this is just a precondition check.                                          |

For a full reconstruction of one operation, grab its trace id from
`outpost`'s output (or the response JSON) and run:

```bash
journalctl --user -u outpost.service --grep req=<id>
```

See [`specs/10-logging.md`][10] for the log schema.

## Tearing it down

To stop using a remote without uninstalling outpost:

```ini
[remote]
host =       # blank disables remote sync
```

Then `outpost reload`. The daemon stays up, the FUSE mount stays up,
session streaming pauses, and no traffic leaves the box. Bare mirrors
and logs on the remote are untouched — clean them up by hand if you
want.

To fully uninstall, see [`scripts/uninstall.sh`][un].

[05]: ../specs/05-ssh-transport.md
[10]: ../specs/10-logging.md
[un]: ../scripts/uninstall.sh
