# 02 — FUSE overlay [locked]

## Goal

`~/.claude/` looks and behaves like a normal directory to every Claude tool.
Every read and write is mirrored to a real directory on disk. The daemon
additionally injects a small set of *virtual* files that the daemon itself
serves.

## Layout

```
~/.claude/                         (FUSE mountpoint)
├── projects/                      passthrough → backing/projects/
│   └── -home-user-myrepo/         passthrough; contains .jsonl session files
├── commands/                      OVERLAY (merge: backing + virtual)
│   ├── send-away.md               virtual (daemon)
│   ├── sync-status.md             virtual (daemon)
│   ├── bring-back.md              virtual (daemon)
│   └── <user's own commands>      passthrough
├── todos/                         passthrough
├── shell-snapshots/               passthrough
├── statsig/                       passthrough
└── …                              everything else: passthrough
```

The *backing dir* (default `~/.local/share/outpost/data/`) has the same
shape, minus the virtual entries.

## Loopback semantics

- All filesystem operations (`open`, `read`, `write`, `mkdir`, `unlink`,
  `rename`, `chmod`, `chown`, `truncate`, `readlink`, `symlink`, `link`,
  `statfs`, xattrs, locks) pass straight through to the corresponding path
  in the backing dir.
- File handles map 1:1 to backing-dir file descriptors. Writes are
  synchronous from the caller's perspective (no daemon-side buffering).
- inode numbers come from the backing filesystem so tools like `find -inum`
  or `tar` behave normally.
- The mount is single-user: only the owning UID can read. `allow_other` is
  off by default.
- The mount is always read-write. No readonly mode.
- The daemon does not probe the backing filesystem type. If the user puts
  the backing dir on NFS / sshfs / etc., that's their call; we don't
  refuse to mount.

## Virtual entries

Virtual entries are read-only, in-memory files served by the daemon. v1 set:

| Path                       | Purpose                                          |
| -------------------------- | ------------------------------------------------ |
| `commands/send-away.md`    | `/send-away` slash command markdown              |
| `commands/sync-status.md`  | `/sync-status` slash command markdown            |
| `commands/bring-back.md`   | `/bring-back` slash command markdown (see 07)    |

Each virtual file:

- Has mode `0444`, owner = the daemon's UID.
- Has `mtime` = daemon start time. (Stable mtime keeps editors/caches happy.)
- Returns the same bytes on every `read`. Content is baked into the binary.
- Cannot be `write`, `unlink`, `rename`, or `chmod`'d. The daemon returns
  `EROFS` for those.

## Overlay merge rules

For `commands/`:

- **Lookup**: try the backing dir first; on `ENOENT`, fall back to the
  virtual table.
  *Consequence*: a user-authored `commands/send-away.md` shadows the
  virtual one. This is intentional — the user can override our default
  slash command.
- **Readdir**: list all backing entries; then append any virtual names that
  weren't already present.
- **Create / write**: writes to `commands/foo.md` always land in the backing
  dir. Once a real `send-away.md` exists, the virtual one is hidden.

For every other top-level subdir: pure passthrough, no overlay logic.

## Write-stream hook for session files

Writes that target `projects/<munged>/*.jsonl` are passthrough as
normal (they hit the backing dir synchronously and return), and *also*
get forwarded to the sync engine as `(munged, id, offset, bytes)`
events. The sync engine's streaming worker batches these and pushes
them to the remote over the shared `ssh.Client`. The hook is
signal-only — the FUSE layer never blocks on the network. See
[06 §"Continuous session streaming"](06-project-sync.md) for the
streaming logic.

## Things this filesystem deliberately does NOT do

- No journaling, no copy-on-write snapshotting. The backing dir is the only
  durable storage.
- No content rewriting. We don't inspect or modify Claude's `.jsonl` files
  in flight. (If we want sync hooks, we tap them via fsnotify on the
  backing dir, not in the FUSE write path — see 06.)
- No caching layer beyond what the kernel does for FUSE. Attribute and
  entry cache timeouts default to 1 second.

## First-run / migration

If `~/.claude/` exists with content before the daemon is installed, the
install script (08) moves that content into the backing dir and then
mounts the FS empty-but-overlaid. Daemon refuses to mount if:

- The mountpoint is non-empty *and* the backing dir is empty (would lose
  user data via shadowing).
- The mountpoint is itself inside the backing dir (loop).

Uninstall is the inverse: stop the daemon, unmount, move backing-dir
contents back into `~/.claude/`, remove the empty backing dir. The user
ends up with `~/.claude/` exactly the way they would have had it if
outpost had never been installed.

See [08-systemd-and-install.md](08-systemd-and-install.md) for both the
install and uninstall scripts' exact steps.

## Library choice

`github.com/hanwen/go-fuse/v2`. Reasons over `bazil.org/fuse`:

- Actively maintained.
- Ships a tested loopback example (`fs.LoopbackNode`) we extend with a
  `NewNode` hook to override only `commands/`.
- Has `fs.MemRegularFile` for clean read-only virtual files.

