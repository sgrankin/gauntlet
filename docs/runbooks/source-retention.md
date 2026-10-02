# Source retention

Linear landing commits do not reach the original PR or change commits. Gauntlet
keeps local audit refs under `refs/gauntlet/source/` and imports forge heads under
`refs/gauntlet/reviews/`. Those inputs may also be needed by delayed hooks.

Use offline maintenance to expire these refs safely:

1. Drain and stop the daemon, including its hooks.
2. Preview sources unused for thirty days:

   ```sh
   gauntlet prune-sources -config /etc/gauntlet/gauntlet.kdl \
       -state /var/lib/gauntlet -retention 720h
   ```

3. Apply the same policy by adding `-apply`, then restart the daemon.

The command takes the daemon's state lock and refuses while the daemon holds
it. Preview does not change refs or retention clocks. Applying removes expired
local audit and review-cache refs with an expected old SHA; it never changes
the remote, normalized target commits, history records, or queue requests.
The next admission poll refetches any current review heads it needs.

Age is time since an input was last fetched or used for a linear trial, not its
commit author date. Retention clocks live in the bare repo's `source-retention`
directory. Archives created before clocks existed are retained: their full
retention window begins on the first `-apply`. Old clocks whose refs already
vanished are expired too.

This releases reachability; it does not run Git GC. Actual disk reclamation
follows ordinary Git garbage collection, and inputs still reached by other refs
remain available. Automatic live pruning needs explicit lifetime pins for every
source consumer; it is deferred so a running or backlogged hook cannot lose its
input. Run this command as scheduled maintenance during a stopped-daemon window.
