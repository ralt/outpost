---
description: Push this project to your outpost remote and continue there headlessly.
---

Run this and show me the output verbatim:

```bash
outpost send-away
```

If it returns successfully, tell me which sessions were launched
headlessly and their PIDs. Then **stop**. Don't take any more turns
in this local session. The remote agent is taking over; anything
written to this `.jsonl` after this point will be discarded when I
run `bring-back`.
