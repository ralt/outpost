# outpost specs

Source-of-truth specs for the `outpost` daemon. Implementation is on hold
until these are reviewed; code will be generated from the specs once they're
agreed.

## Read in this order

| # | Doc | What it covers |
| - | --- | -------------- |
| 00 | [overview.md](00-overview.md) | Problem, goals, non-goals, glossary |
| 01 | [architecture.md](01-architecture.md) | Components, processes, data flow |
| 02 | [fuse-overlay.md](02-fuse-overlay.md) | Mount layout, backing dir, virtual entries |
| 03 | [config.md](03-config.md) | Config file schema and defaults |
| 04 | [control-and-cli.md](04-control-and-cli.md) | Daemon ↔ CLI IPC and command surface |
| 05 | [ssh-transport.md](05-ssh-transport.md) | Persistent SSH connection, multiplexing, git transport |
| 06 | [project-sync.md](06-project-sync.md) | Project detection, remote layout, what gets synced |
| 07 | [send-away.md](07-send-away.md) | End-to-end send-away flow and resumption |
| 08 | [systemd-and-install.md](08-systemd-and-install.md) | User unit, install, first-run migration |
| 10 | [logging.md](10-logging.md) | Sinks, levels, components, per-RPC trace ids |

## How we iterate

1. Read top-to-bottom. Each doc is short and scoped.
2. Mark spots you disagree with — edit in place, or leave a comment like
   `> NACK: ...` inline.
3. When a spec section is settled, prefix its heading with `[locked]`.
   Anything not locked is fair game to rewrite.

## Status legend

Throughout the specs you'll see status tags after headings:

- `[locked]` — agreed, do not change without discussion
- `[draft]` — my best guess, expect edits
