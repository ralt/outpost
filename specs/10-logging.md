# 10 — Logging [locked]

## What logs are for

After-the-fact debugging. "Why did the last send-away fail?", "When did
the background mirror push last succeed?", "Did fsnotify see the new
project?". Logs are not for normal usage — `outpost status` covers
that — they're for the moment something has gone wrong.

## Destinations

**stderr only.** The daemon writes one line per event to stderr and stops
there — persistence is the supervisor's job:

- Under `systemd --user`, journald captures it. Tail via
  `journalctl --user -u outpost.service -f`. If the user wants a
  durable file copy, that's a journald-config concern
  (`SystemMaxUse=`, `Storage=persistent`, or a journal forwarder), not
  ours.
- When run by hand (`outpost daemon` in a terminal), the user
  sees it directly and can redirect.

No in-daemon file sink, no rotation logic, no `daemon.log`. One fewer
moving part.

## Format

Default: human-readable single-line:

```
2026-05-19T18:33:02.118Z INFO  ssh        connected host=alice@dev-box.example.com
2026-05-19T18:33:02.421Z INFO  sync       discovered project=-home-alice-Git-…-outpost git=true
2026-05-19T18:33:07.913Z INFO  sync       bare-init+mirror project=…outpost refs=14
2026-05-19T18:33:09.002Z INFO  sync       worktree-add project=…outpost branch=feature/x
2026-05-19T18:42:11.504Z INFO  ctl  req=h7g3k2pq send-away project=…outpost session-count=2
2026-05-19T18:42:11.812Z DEBUG ctl  req=h7g3k2pq mirror-push refs=2 bytes=14823
2026-05-19T18:42:12.337Z INFO  ctl  req=h7g3k2pq worktree-reset branch=feature/x sha=a3f9c1
2026-05-19T18:42:12.601Z INFO  ctl  req=h7g3k2pq uncommitted bytes=4112 hunks=12 untracked=3
2026-05-19T18:42:13.221Z INFO  ctl  req=h7g3k2pq agent-launched session=4f8a2e1c pid=31204
2026-05-19T18:42:13.227Z INFO  ctl  req=h7g3k2pq done dur=1.723s
```

Columns: RFC3339-nano timestamp, level, component, optional `req=<id>`,
free-form message + key=value pairs.

JSON output via `logging.format = json` (one JSON object per line,
using `log/slog`). For machine ingestion only; the human-readable
default is the supported way to read logs.

## Levels

`log/slog`'s standard four:

| Level | When to use                                                                             |
| ----- | --------------------------------------------------------------------------------------- |
| ERROR | Something visibly failed (RPC error returned, ssh disconnected, FUSE mount lost).       |
| WARN  | Recovered automatically, but worth noticing (ssh reconnect, mirror push conflict, dirty remote worktree at send-away). |
| INFO  | Normal lifecycle events (startup, mount, project discovered, mirror push success, send-away success). |
| DEBUG | Per-event detail (every fsnotify event, every RPC payload, individual git/ssh command lines). |

Default level is `INFO`. Override:

- Config: `logging.level = debug` in `config.ini`. Takes effect on
  daemon start or `outpost reload`.
- Env: `OUTPOST_LOG_LEVEL=debug` wins over config when set. Useful
  for one-off foreground runs.

## Components

Component is a short tag identifying the subsystem:

| Component | Owns                                                       |
| --------- | ---------------------------------------------------------- |
| `fuse`    | FUSE server + virtual-command overlay.                     |
| `ctl`     | Control socket: accept, RPC dispatch, RPC handler.         |
| `ssh`     | `ssh.Client` lifecycle, connect/disconnect, keepalives.    |
| `sync`    | Project watcher + periodic scheduler + git push pipelines. |
| `daemon`  | Process-level: startup, signal handling, shutdown.         |
| `config`  | Config load/reload.                                        |

Every log line has exactly one component tag.

## Per-RPC trace ids

Every control-socket RPC gets an 8-char base32 request id assigned at
accept time and carried through every log line produced for that RPC
(`req=<id>`). The request id is also returned in the RPC response so a
CLI invocation that prints "request id h7g3k2pq" gives the user an
exact `journalctl --grep` key. This makes
`journalctl --user -u outpost.service --grep req=h7g3k2pq` show
the complete story of one send-away call.

Background scheduler runs get `task=<id>` instead of `req=` (same
8-char format).

## What does NOT go in logs

- File contents of any kind. We ship patches and tarballs over ssh; the
  bytes never enter the logger.
- Claude session `.jsonl` content. We copy these files; we never parse
  or quote them.
- SSH passphrases. The daemon never sees these (passphrased keys must
  be added to ssh-agent).
- Host key *material*. The daemon sees host keys (it verifies them
  in-process), but only the fingerprint is logged on TOFU pin — never
  the full key bytes.
- Anything from the user's transcripts. The daemon doesn't read them.

We DO log:

- Project names (the `-home-…-outpost` strings). The user already
  exposed these to us; they're not sensitive.
- Remote hostname and username (`alice@dev-box.example.com`).
- File counts and byte counts for shipments — never filenames of
  untracked files (those can be sensitive).

## CLI sugar

`outpost logs`:

- If the daemon is running under systemd, execs into
  `journalctl --user -u outpost.service -f`. `--since 10m` and
  `--req <id>` translate to the equivalent `journalctl` flags
  (`--since`, `--grep`).
- Otherwise, prints a message explaining where stderr went (e.g.
  "running in foreground; logs are on your terminal") and exits.

We don't try to tail any other source — there isn't one.

## Config knobs

The `[logging]` section in `config.ini` controls level and format.
Canonical schema and defaults live in [03-config.md](03-config.md).
