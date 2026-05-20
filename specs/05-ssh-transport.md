# 05 — SSH transport [locked]

## Requirement

The prompt is explicit:

> on top of the open SSH connection, you do a git clone from the local one
> to the remote SSH folder

So the daemon must keep one SSH connection alive and route every
remote operation (exec, file copy, git push, session streaming)
through it.

## Approach

The daemon owns a single in-process
`golang.org/x/crypto/ssh.Client`. Every remote operation opens an
`ssh.Session` (channel) on that client. One TCP, one auth, lifetime
of the daemon.

```
daemon process
  └── ssh.Client ──TCP──► remote sshd
        ├── ssh.Session: git-receive-pack <bare>  (go-git mirror push)
        ├── ssh.Session: git -C <cwd> switch / reset / apply
        ├── ssh.Session: tar -x -C <cwd>          (untracked tar in)
        ├── ssh.Session: tar -c <remote_cwd>      (untracked tar out, bring-back)
        ├── ssh.Session: nohup claude -p …        (headless agent launch)
        ├── ssh.Session: kill <pid>               (agent stop on bring-back)
        └── sftp.Client over a session            (session-file streaming + fallback uploads)
```

Why in-process `x/crypto/ssh` + go-git rather than the OpenSSH binary
+ shelled-out git: the daemon is the *only* thing that talks to the
remote, so ControlMaster's "multiple processes share a socket"
benefit doesn't apply. go-git plugs straight into our `ssh.Client` as
its transport, so push and sftp share the same connection. Tradeoff:
we lose `~/.ssh/config` directives (`ProxyJump`, host aliases, custom
algorithms). Users with complex configs set the remote directly in
`config.ini`.

## What runs over the shared client

| Operation                                  | Direction | Mechanism |
| ------------------------------------------ | --------- | --------- |
| Mirror push (background + send-away catch-up) | local → remote | **go-git** `Remote.Push` with refspec `+refs/*:refs/*`; transport opens `git-receive-pack` as an `ssh.Session` on the shared client. |
| Bare init / `worktree add` / `switch` / `reset --hard` | exec on remote | `ssh.Session.Run("git …")`. |
| Apply uncommitted patch                    | local → remote | `ssh.Session("git -C <cwd> apply --index --whitespace=nowarn")` with `git diff --binary HEAD` (local) on stdin. |
| Tar of untracked files                     | local → remote | `ssh.Session("tar -x -C <cwd>")` with local `tar -c …` on stdin. |
| Session-file streaming                     | local → remote | FUSE write hook → daemon streaming worker → `sftp.WriteAt(...)` (100ms debounce, 1 MB ring buffer). |
| Session-file fallback upload               | local → remote | `sftp` full upload during inline bootstrap or buffer-overflow recovery. |
| Launch headless agent                      | exec on remote | `ssh.Session.Run("cd <cwd> && nohup claude -p … --resume <id> --allowedTools '…' > <log> 2>&1 < /dev/null & echo $!")`; PID captured from stdout. |
| Stop headless agent                        | exec on remote | `ssh.Session.Run("kill <pid>")`, then `kill -9` after grace period. |
| Bring-back: fetch refs                     | remote → local | **go-git** `Remote.Fetch` over an `ssh.Session` opening `git-upload-pack`. |
| Bring-back: capture remote uncommitted     | remote → local | `ssh.Session("git -C <cwd> diff --binary HEAD")` and `ssh.Session("git -C <cwd> ls-files --others --exclude-standard -z | tar -c --null -T -")`, each piping stdout into a local consumer (`git apply --index` and `tar -x`, both shelled out — local computation). |
| Bring-back: session-file pull              | remote → local | `sftp` download of every `.jsonl` in `~/.claude/projects/<munged>/`. |

All of these run on the same `ssh.Client`. No additional TCP
handshakes.

## What we shell out for, and why

These are **local computations**, not protocol steps; go-git doesn't
help and shelling out gets us the user's exact git config:

- `git diff --binary HEAD` — produces the uncommitted-state patch.
  go-git doesn't expose a clean API for binary diff generation.
- `git ls-files --others --exclude-standard -z` — lists untracked-
  not-ignored paths; piped into a local `tar` to produce the stream
  we ship.
- `git apply --index --whitespace=nowarn` and `tar -x` — applied on
  the *local* side during bring-back, by the same logic.

## Auth resolution

In order:

1. If `remote.identity_file` is set, load it (PEM-decoded). No
   passphrase prompts; passphrased keys must be added to ssh-agent.
2. Else `SSH_AUTH_SOCK` (talk to ssh-agent).
3. Else look for default key files (`~/.ssh/id_ed25519`,
   `~/.ssh/id_rsa`), in that order.

If nothing produces a usable signer, the daemon reports
`AUTH_NO_KEYS` via `status.last_error` and refuses to mark itself
connected.

## Host key verification (TOFU)

Trust On First Use against `remote.known_hosts_file` (default
`~/.ssh/known_hosts`). The daemon wraps
`golang.org/x/crypto/ssh/knownhosts.New(...)` with a callback that
discriminates the two failure modes:

- **Unknown host** (`KeyError.Want` empty): no record for this host.
  The daemon pins the key with `knownhosts.Line(...)` and accepts.
  Logged at INFO: `host-key-pinned host=… fingerprint=SHA256:…`.
- **Mismatch** (`KeyError.Want` non-empty): record exists, key
  differs. **Always rejected** — surfaces as `KNOWN_HOSTS_MISMATCH`
  in `status.last_error`. TOFU is "trust on *first* use", not "trust
  forever"; a key change after pinning is treated as possible MITM
  and the user resolves manually (e.g. `ssh-keygen -R <host>` +
  re-pin).

Strict-no-TOFU is reachable by pre-populating `known_hosts` so the
file already has the record on first connection; TOFU never fires.

If the known-hosts file doesn't exist, the daemon creates it (`0600`,
owner = the user) before pinning.

## Post-connect: `$HOME` check

Right after auth, the daemon opens an `ssh.Session`, runs
`echo $HOME`, and compares to local `os.Getenv("HOME")`:

- Match → cache as `remote.home`; mark `remote.connected = true`.
- Mismatch → close the connection; set `remote.last_error =
  HOME_MISMATCH: local=<local> remote=<remote>`; mark
  `remote.connected = false`. The reconnect loop keeps retrying so
  the user can fix their remote setup without restarting the daemon.

Rationale: project sync requires byte-identical cwd on both sides.
See [06 §"Requirement: matching $HOME"](06-project-sync.md).

## Failure modes

| Symptom                                | Surface                                       |
| -------------------------------------- | --------------------------------------------- |
| Network blip; client read fails        | Auto-reconnect with backoff; `status.connected=false` window. |
| Auth failure                           | Fatal for the SSH layer; daemon keeps running; `status.last_error` populated. |
| Host key mismatch                      | Hard fail; `KNOWN_HOSTS_MISMATCH` in `status.last_error`. |
| Remote `$HOME` differs                 | Hard fail; `HOME_MISMATCH` in `status.last_error`. |
| Remote disk full during push           | `send-away` returns `REMOTE_DISK_FULL`. |
| Remote git rejects push                | `send-away` returns `REMOTE_REJECT`, with diff. |
| `claude` not on remote PATH            | `send-away` returns `REMOTE_MISSING_BIN`. |
| Patch fails to apply                   | `send-away` returns `APPLY_FAILED`, with rejected hunks. |

## Keepalive and reconnect

- Every `remote.keepalive_interval` seconds, the daemon sends a
  `keepalive@openssh.com` global request. Missed replies trigger a
  reconnect.
- Reconnect dials a fresh `ssh.Client`, runs auth + known-hosts +
  `$HOME` checks, and re-publishes the client to the sync engine.
- In-flight operations on the old client see EOF and are retried by
  their callers: background scheduler retries with exponential
  backoff; foreground send-away/bring-back surface the error.

## Connection lifecycle

- `remote.host` empty → no client; background scheduler and streaming
  worker are idle.
- `remote.host` set → client dialled at daemon boot and kept hot, so
  the send-away hot path has no handshake cost.
- On daemon shutdown, `ssh.Client.Close()` closes the connection
  cleanly.
