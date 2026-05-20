# outpost

Hand off your Claude Code session to a remote machine so it can keep
working headlessly while you're away, then bring the result back when
you're ready.

> **🚧 Very early software.** Outpost is currently *spec'd*, not built —
> the [`specs/`](specs/) directory describes the design end-to-end; the
> code has not been written yet. If you found this repo expecting to
> `go install` something, check back later.

I'm pairing with Claude on my laptop. I need to step away — close the
lid, go to a meeting, get on a flight. I want the agent to keep working
**autonomously** on my configured dev box, picking up exactly where we
left off. When I'm back, I run `bring-back` and resume work locally with
the remote's progress applied. The agent on the remote is fully
headless — there's nothing to attach to, by design. "Work while I'm
away" is the whole point.

That's the user story. Outpost is the daemon that makes it happen.

## In a nutshell

- A small Go daemon, started by `systemd --user` on login.
- Mounts a transparent FUSE overlay on `~/.claude/`, so Claude Code
  keeps working exactly as today — every read and write passes
  through to a real backing directory on disk.
- Continuously mirrors each Claude project's git refs and live
  session writes to a configured SSH remote, over a single
  persistent connection.
- Exposes two virtual slash commands inside Claude:
  - `/send-away` — flip the project to "remote owns it", launch a
    headless `claude -p --resume` over `nohup` on the remote, and
    walk away.
  - `/bring-back` — stop the remote agent, fetch the work it did,
    apply its uncommitted state locally, and resume.

The remote agent runs without a terminal — no tmux, no PTY, nothing
to attach to. It just works in the background until the task is done
or until you bring it back.

## Status

| Area                                         | State        |
| -------------------------------------------- | ------------ |
| Design specs                                 | ✅ Locked    |
| Go module + skeleton                         | 🚧 Pending   |
| FUSE passthrough + virtual `commands/` overlay | 🚧 Pending |
| SSH client + go-git push transport           | 🚧 Pending   |
| Project watcher + background mirror push     | 🚧 Pending   |
| Continuous session-file streaming            | 🚧 Pending   |
| `send-away` / `bring-back` pipeline          | 🚧 Pending   |
| Uncommitted-delta ship/apply                 | 🚧 Pending   |
| systemd user unit + install script           | 🚧 Pending   |

Nothing is runnable yet.

## How it works

Read the specs end-to-end if you're curious. Top-to-bottom in
[`specs/`](specs/):

| #  | Doc                                                | Topic                                                |
| -- | -------------------------------------------------- | ---------------------------------------------------- |
| 00 | [overview](specs/00-overview.md)                   | Problem, goals, non-goals, glossary                  |
| 01 | [architecture](specs/01-architecture.md)           | Components, processes, data flow                     |
| 02 | [fuse-overlay](specs/02-fuse-overlay.md)           | FUSE mount, virtual commands, write-stream hook      |
| 03 | [config](specs/03-config.md)                       | `config.ini` schema and defaults                     |
| 04 | [control-and-cli](specs/04-control-and-cli.md)     | Daemon ↔ CLI IPC and command surface                 |
| 05 | [ssh-transport](specs/05-ssh-transport.md)         | Single in-process `ssh.Client`, go-git over it       |
| 06 | [project-sync](specs/06-project-sync.md)           | Project ownership, mirror push, session streaming    |
| 07 | [send-away](specs/07-send-away.md)                 | End-to-end send-away/bring-back flow                 |
| 08 | [systemd-and-install](specs/08-systemd-and-install.md) | User unit, install/uninstall, migration         |
| 10 | [logging](specs/10-logging.md)                     | Stderr+journald, levels, components, trace ids       |

## Key design choices

A few things that aren't obvious from the name:

- **Matching `$HOME` required.** Claude's session files have absolute
  paths baked in. For `claude --resume` to work on the remote, the
  remote cwd must be byte-identical to local; we check `echo $HOME`
  at connect time and refuse to operate on a mismatch.
- **One SSH connection, period.** `golang.org/x/crypto/ssh` plus
  go-git's push transport, all multiplexed onto a single TCP +
  single auth handshake. No ControlMaster, no shelling out to git
  for protocol.
- **TOFU on host keys.** First connection pins the remote's host key
  to `~/.ssh/known_hosts`; subsequent mismatches are hard-rejected.
- **Local writes during `owner=remote` are noise.** They're not
  blocked, but they're discarded on `bring-back`. The CLI prompts
  y/N before destroying anything.

## License

TBD. (Pre-implementation; happy to revisit when there's code.)
