---
description: Show the outpost daemon's view of this project and the remote.
---

Run this and show me the output verbatim:

```bash
outpost status
outpost projects
```

These read from the daemon's in-memory state — they never touch the
remote, so they're safe to call at any time.
