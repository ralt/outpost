# 01 — Architecture [locked]

## Processes

There is exactly one long-lived process per user session:

```
   systemd --user
        │ starts
        ▼
   outpost daemon ──┬─► FUSE mount (~/.claude → backing dir)
                       ├─► Control socket (unix, $XDG_RUNTIME_DIR/outpost.sock)
                       └─► SSH client (one persistent connection to remote)
```

CLI invocations (`outpost send-away`, `outpost status`, …) are
short-lived clients that talk to the daemon's control socket. They never do
FUSE or SSH work themselves.

## Components inside the daemon

```
┌──────────────────────── outpost daemon ──────────────────────────┐
│                                                                      │
│  ┌──────────────┐  ┌────────────────┐  ┌────────────────┐            │
│  │ FUSE server  │  │ Control server │  │  SSH client    │            │
│  │  (go-fuse)   │  │ (unix socket)  │  │ (x/crypto/ssh, │            │
│  │              │  │  JSON-line RPC │  │  go-git push   │            │
│  │              │  │                │  │  on same conn) │            │
│  └──────┬───────┘  └────────┬───────┘  └───────┬────────┘            │
│         │                   │                  │                     │
│         │ Loopback +        │ RPCs:            │ One TCP + one auth  │
│         │ commands overlay  │  send-away       │ shared across exec, │
│         │                   │  bring-back      │ sftp, and go-git    │
│         │                   │  status          │ mirror push.        │
│         │                   │  projects        │                     │
│         ▼                   ▼                  ▼                     │
│  ┌────────────────────────────────────────────────────────────────┐  │
│  │                       Sync engine                              │  │
│  │  ┌─────────────────────┐    ┌───────────────────────────────┐  │  │
│  │  │ Project watcher     │    │ Periodic scheduler            │  │  │
│  │  │ - fsnotify on       │───►│ - init bare mirror on discover│  │  │
│  │  │   <backing>/projects│    │ - refresh tick (default 1h):  │  │  │
│  │  │ - emits "discovered │    │     git push --mirror to each │  │  │
│  │  │   <project>" events │    │     project's bare mirror     │  │  │
│  │  └─────────────────────┘    └───────────────────────────────┘  │  │
│  │  ┌────────────────────────────────────────────────────────────┤  │
│  │  │ Send-away pipeline (triggered by RPC)                      │  │
│  │  │ - catch-up mirror push (usually no-op)                     │  │
│  │  │ - drain session stream + pause it                          │  │
│  │  │ - worktree reconcile (switch + reset --hard) on remote     │  │
│  │  │ - ship uncommitted delta (staged + unstaged + untracked)   │  │
│  │  │ - flip owner to remote                                     │  │
│  │  │ - launch headless `nohup claude -p …` per session          │  │
│  │  └────────────────────────────────────────────────────────────┘  │
│  └────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────┘
```

Cloning and ref sync run **in the background** as soon as the daemon
notices a new project, and again every `sync.background_interval`
thereafter. `send-away` is therefore *small* — it only has to catch up
any refs the scheduler hasn't picked up yet, reconcile the worktree,
ship the uncommitted delta, and launch headless agents. Detail in
[06-project-sync.md](06-project-sync.md).

Every component logs to a shared structured logger; see
[10-logging.md](10-logging.md) for sinks, levels, components, and
per-RPC trace ids.

## Data flow: a normal Claude session

1. User runs `claude` in some shell.
2. Claude opens files under `~/.claude/`. Reads/writes go to the FUSE mount.
3. FUSE server passes everything through to the backing dir (loopback).
4. `~/.claude/commands/` also exposes a few virtual `.md` files contributed
   by the daemon, alongside any user-created commands.

In parallel, regardless of whether the user has touched `send-away` yet:

- The **project watcher** sees a new directory appear in
  `<backing>/projects/<munged>/` (via fsnotify) and emits a *discovered*
  event for it.
- The **periodic scheduler** picks up that event and, *if* the cwd
  reconstructed from the project name resolves to a git repo, initialises
  a bare repo on the remote and pushes `--mirror` from the local tree.
  All branches and tags. Then materialises a linked worktree at the
  same absolute path as the local cwd (the daemon requires matching
  `$HOME`; see [06](06-project-sync.md)). Runs on the shared `ssh.Client`.
- Every `sync.background_interval` (default 1h), the scheduler walks
  known projects and runs `git push --mirror` to keep the remote bare
  repo current with every local ref. The worktree is *not* touched in
  this loop — it's refs only.

Result: by the time the user ever asks for `send-away`, the remote
bare mirror has every local ref and the worktree exists at the
reproduced path.

## Data flow: project discovery + periodic refresh

1. fsnotify on `<backing>/projects/` fires on `IN_CREATE`.
2. Watcher emits `{name, path}` to the scheduler queue.
3. Scheduler dedups (we may get multiple events per project as Claude
   writes files into it) and once per project decides:
   - If the reconstructed path is not a git repo → mark project as
     `non-git`, skip from background sync, but it stays eligible for
     `bring-back`/`projects` listing.
   - If it is a git repo → push to a per-daemon work queue.
4. Worker pops from the queue, holds the SSH connection, and either:
   - First time:
     a. `git init --bare <remote-bare-path>` (over ssh).
     b. `git push --mirror <ssh-url-to-bare>` from the local tree.
     c. `git -C <remote-bare-path> worktree add <remote_path> <local_branch>`
        to materialise the working tree at the reproduced path.
   - Subsequent: `git push --mirror <ssh-url-to-bare>`. Mirror pushes
     all refs (branches + tags) and prunes refs that no longer exist
     locally. The worktree is left alone in this path.
5. Result recorded in the project's `.meta` file on the remote.
6. Failures: logged, surfaced through `outpost status`, retried with
   exponential backoff. They do **not** block `send-away`; send-away
   does its own (now-small) push retry inline so the user sees a
   meaningful error rather than a stale background failure.

## Data flow: send-away

1. In Claude, the user types `/send-away`.
2. Claude reads `~/.claude/commands/send-away.md` (a virtual entry served by
   the daemon). The markdown tells Claude to run `outpost send-away`.
3. Claude executes that shell command. The CLI connects to the control
   socket and issues a `send-away` RPC.
4. The daemon's sync engine:
   a. Identifies the current project (cwd-derived name → project dir).
   b. Resolves the project's working tree on the host filesystem.
   c. Confirms the working tree is a git repo (else returns an error
      explaining why we won't sync).
   d. Catch-up `git push --mirror` to pick up any refs the scheduler
      hasn't reached yet (no-op in the common case), then reconciles
      the remote worktree to the local branch + commit
      (`git switch` + `git reset --hard` over ssh).
   e. Ships the **uncommitted delta**: staged + unstaged + untracked
      (untracked filtered through `.gitignore`).
   f. Copies every session `.jsonl` from the project dir to the remote.
   g. Launches one **headless** `nohup claude -p '<prompt>' --resume <id>`
      per session, backgrounded; PIDs captured into `.meta/`. No tmux,
      no PTY — the agent runs autonomously until done.
5. CLI prints a one-line summary with the agent PIDs.
6. Claude tells the user to walk away and come back with
   `outpost bring-back`. There's nothing to attach to.

## Why one connection

The prompt is explicit: "on top of the open SSH connection, you do a git
clone from the local one to the remote SSH folder". Reasons:

- Connection setup (TCP + KEX + auth) is the slow part. Reusing pays off
  immediately for the multi-step send-away flow.
- Some networks rate-limit fresh SSH handshakes. One long-lived connection
  avoids that class of problems.
- A single connection gives us a single place to surface health / errors
  / reconnect logic in the daemon UI.

See [05-ssh-transport.md](05-ssh-transport.md) for how multiplexing works.

## Process lifetime

- Daemon starts on `systemd --user` session start.
- It mounts the FUSE filesystem before signalling `READY=1`; the unit is
  `Type=notify` so subsequent units (or the user shell) can rely on the
  mount being up.
- On `SIGTERM` (logout, manual stop), the daemon unmounts cleanly and
  exits. Unmount uses `fusermount -u` semantics so stale mounts don't
  survive.
- If the daemon crashes, the user's next access to `~/.claude/…`
  surfaces a "Transport endpoint is not connected" error. The systemd
  unit has `Restart=on-failure`; spec 08 covers the recovery flow.
