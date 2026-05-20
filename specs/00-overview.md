# 00 — Overview [locked]

## Problem

When you run `claude` on your laptop, all of Claude Code's state — session
transcripts, todos, shell snapshots, slash commands — lives under `~/.claude/`.
If you close the laptop, that work stops. There is no built-in way to hand the
running agent off to a server that stays online.

`outpost` is a user-level daemon that fixes that, without forking or
patching Claude Code itself. It does two jobs:

1. **Stay out of the way.** Mount a transparent FUSE filesystem over
   `~/.claude/` so the existing CLI keeps working exactly as today — every
   read and write is reflected to a real backing directory on the same disk.
2. **Add a remote escape hatch.** Over a single, persistent SSH connection
   to a configured host, the daemon can mirror a project's working tree and
   its Claude session to that host, then resume the agent there.

The daemon also surfaces new slash commands (e.g. `/send-away`) under
`~/.claude/commands/` by overlaying them into the FUSE mount, so the user
triggers the daemon by typing slash commands in Claude itself — no extra
terminal needed.

## Goals (v1)

- Transparent FUSE passthrough of `~/.claude/`. Claude Code, run from any
  shell, sees and writes exactly what it sees today.
- Single config file. One SSH endpoint per user.
- One persistent SSH connection, reused for all remote operations (no
  per-operation handshakes).
- `/send-away` slash command that, for the current project:
  - Ensures the project's git repo is cloned on the remote, with the
    working tree at the **exact same absolute path** as locally. The
    daemon requires matching `$HOME` between local and remote (in
    practice: same username, same OS family); see
    [06-project-sync.md §"Requirement: matching $HOME"](06-project-sync.md).
  - Sends tracked changes + untracked-but-not-ignored files.
  - Sends **every** Claude session file for the project (one Claude
    instance per session).
  - Launches one **headless** agent (`nohup claude -p '<prompt>'
    --resume <session>`) per session file, in the reproduced working
    tree. The agent runs autonomously — there is nothing to attach to.
    The user comes back later and runs `bring-back` to apply remote
    progress locally.
- `systemd --user` service that starts the daemon on login and mounts
  the FS before Claude is likely to be invoked. See
  [08-systemd-and-install.md](08-systemd-and-install.md).

## Non-goals (v1)

- Multi-host fan-out. One remote at a time.
- Encrypted-at-rest backing storage. Backing is a normal directory on disk.
- Real-time bidirectional sync. Sync is triggered by `/send-away` (push) and
  an explicit pull command (see 07); not a continuous mirror.
- Conflict resolution UI. Remote-wins or local-wins is a config knob; no
  three-way merging in v1.
- Windows / macOS. Linux + FUSE only. (macOS could work via `macFUSE` later.)
- Patching Claude Code. The daemon never modifies Anthropic's binary or its
  on-disk format.

## Glossary

- **Backing dir** — the real on-disk directory whose contents the FUSE mount
  mirrors. Default: `~/.local/share/outpost/data/`.
- **Mountpoint** — where the FUSE filesystem appears. Default: `~/.claude/`.
- **Project (Claude sense)** — a subdirectory of `<mount>/projects/` whose
  name is the cwd Claude was launched in, with `/` replaced by `-`. e.g.
  `-home-alice-Git-github-com-alice-outpost` maps back to
  `/home/alice/Git/github.com/alice/outpost`.
- **Session file** — a `.jsonl` file inside a project directory containing the
  transcript Claude reads with `claude --resume`. A project may have many.
- **Virtual entry** — a file/dir surfaced by the FUSE layer that does not exist
  in the backing directory; the daemon injects it. Example:
  `commands/send-away.md`.
- **Remote** — the SSH host configured in `config.ini`. One per daemon.
- **Send-away** — push the current project's working tree + every Claude
  session to the remote and start the agents there.
