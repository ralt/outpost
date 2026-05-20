---
description: Pull progress from the outpost remote back to this machine and resume locally.
---

Run this and show me the output verbatim:

```bash
outpost bring-back
```

This is **destructive** toward any local session writes made after
`/send-away` — the remote's `.jsonl` files overwrite the local copies.
The CLI prompts y/N first; pass `--yes` if you've already decided.

After it returns, the remote agent has stopped and its progress is
applied locally. You can resume with `claude --resume <session>`.
