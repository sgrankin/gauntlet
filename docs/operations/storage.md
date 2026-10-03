# State, retention, and backups

Remote Git history is authoritative for successful landings. Local state also
contains durable operator intent and diagnostics; back it up if those must
survive host replacement.

| Data | Retention and recovery |
|---|---|
| Target commits and candidate refs | Remote Git; protect and back up through the hosting provider or mirrors. |
| `queue-controls.json` | Durable pause/emergency controls and bounded audit. Invalid state blocks startup; preserve it across restarts. |
| Review parks and emergency intents | Local state files; preserve rejected revisions and revision-bound emergency requests. |
| SQLite run/check/hook records | No automatic retention. Losing history loses diagnostics and ref park seeds, not proof of landing. |
| Queue-depth samples | Default 14 days; `history { depth-retention ... }`. |
| Full compressed logs | Default 30 days; `log-retention`. Missing/pruned logs return 404. |
| Original source archives and review-cache refs | Default 30 days; `source-retention`. Startup/hourly sweeps exclude active leases. |
| Failure-review observations | Bounded by age and count: 30 days and 10,000 observations. |
| Receipt notes | Append-only; no automatic reaper. A failed target CAS can leave a receipt for a commit that never landed. |

## Automatic source pruning

Pruning runs while the daemon is live. Trials, workers, cancellation cleanup,
and running/backlogged hooks lease their inputs. Expiration releases unused
local refs for normal Git garbage collection; it does not change target history
or remote refs. No stopped-daemon pruning procedure is required.

## Git object collection

Ordinary `git gc` can run on the daemon's bare repository. In-flight chain tips
are pinned with local refs, and the normal grace period protects the short
interval between object creation and pinning. Do not delete live pin refs or
use `git gc --prune=now` against an active repository: immediate pruning can
race object creation.

## Backups

- Preserve remote Git through your normal repository backup process.
- Back up operator configuration, credential provisioning, and durable local
  control state. Keep secrets out of public archives.
- Use SQLite's backup mechanism for a consistent live history backup.
- Logs, exported trees, and caches are replaceable; include them only when the
  diagnostic or recovery value justifies the storage.
