# outpost

Hand off your Claude Code session to a remote machine so it can keep
working headlessly while you're away, then bring the result back when
you're ready.

> **🚧 Early software.** Implementation is in tree (`go build ./...` is
> green) and a full send-away/bring-back round trip works against a Docker
> dev remote (`scripts/dev-remote.sh up`). It is not battle-tested against
> a real Claude session yet. Expect rough edges.

I'm pairing with Claude on my laptop. I need to step away — close the
lid, go to a meeting, get on a flight. I want the agent to keep working
**autonomously** on my configured dev box, picking up exactly where we
left off. When I'm back, I run `bring-back` and resume work locally with
the remote's progress applied. The agent on the remote is fully
headless — there's nothing to attach to, by design. "Work while I'm
away" is the whole point.

That's the user story. Outpost is the daemon that makes it happen.

## What you get

Two slash commands inside Claude:

- **`/send-away`** — flips the project to "remote owns it", launches a
  headless `claude -p --resume` on your dev box, walks away.
- **`/bring-back`** — stops the remote agent, pulls back its progress
  (commits, uncommitted edits, updated session transcript), resumes
  locally.

Under the hood, outpost mounts a **FUSE filesystem** at `~/.claude/` —
every read and write passes straight through to a real directory on
disk at kernel speed. Never in your way, incredibly efficient.

## Install

```bash
./scripts/install.sh
```

That builds outpost, drops a default config at
`~/.config/outpost/config.ini`, and starts it on login. Edit the
`[remote]` section of the config to point at your dev box, then in
Claude type `/send-away`.

For the dev box itself — SSH access, matching `$HOME`, what binaries
need to be on `PATH`, full troubleshooting table — see
**[docs/remote-setup.md](docs/remote-setup.md)**.

## Try it without a real remote

```bash
./scripts/dev-remote.sh up
```

Spins up a Docker container that pretends to be your remote (matching
`$HOME`, a stub `claude` binary, your SSH key already trusted) and
rewrites `~/.config/outpost/config.ini` to point at it. `/send-away`
and `/bring-back` then exercise the whole pipeline locally. Tear it
down with `./scripts/dev-remote.sh down`.

## Heads up

- **Local edits after `/send-away` are discarded** when you run
  `/bring-back`. The remote is the live copy; anything you do locally
  in the meantime — including Claude's own trailing turns — gets
  overwritten. `bring-back` prompts y/N first.

## Design docs

The [`specs/`](specs/) directory has the locked design documents if
you want to know how it works under the hood — FUSE overlay, SSH
transport, sync engine, send-away pipeline, etc.

## License

TBD.
