# Incident controls

Pause/resume is per target, with global dashboard controls. Pause cancels active
verification and stops new work and publication. Its actor, reason, and timestamp
persist in `queue-controls.json`; restart does not resume it. Resuming rebuilds
work against the current target. Shutdown drain is a separate lifecycle operation.

Urgency advances explicitly requested revisions and their requested prerequisites
without preempting current verification. After three urgent landings, eligible
ordinary work gets a turn. Urgency never requests a prerequisite implicitly.
GitHub supports `merge urgent` and `merge stack urgent`; normal writer permission
is required, and the request identity remains in candidate/history metadata.

## Emergency merge

The operator must enable `emergency-merges true`. The dashboard, admin API, CLI,
and authorized GitHub commands can request an exact dependency-valid prefix with
a mandatory reason. This cancels current target work and constructs a new chain.
Validation nodes become **waived**, not passed. Receipt/provenance nodes and their
image builds remain required. Signing, conflict detection, exact constructed
commit identity, source validation, and the target compare-and-swap still apply.
A prefix crossing a check-spec boundary is rejected; select a shorter prefix.

Pause override and check bypass are independent. A request that passes a pause
does not resume the target. `override-pause` alone still runs verification. The dashboard offers **Verify and
merge while paused** for this path; its admin command is `merge-paused` with an
explicit pause override.
`skip-checks` alone cannot pass a pause. On GitHub:

```
@gauntlet merge urgent override-pause -- restore service
@gauntlet merge stack urgent skip-checks -- emergency fix
@gauntlet merge stack urgent skip-checks override-pause -- restore service
```

GitHub emergency requests freeze their refs, heads, review versions, and reason
on first observation, in `github-emergency-intents.json`. Editing the request,
pushing another revision, or changing its selected prefix requires a new comment.
At most 500 intents are retained, subject to a byte bound; older retired command IDs cannot become new
emergencies. A plain merge command still cannot implicitly request ancestors.

Check bypass also waives configured GitHub required-check gates, but keeps
requester authorization, current-head/message checks, approval and conversation
gates, and operator Rego. Gerrit retains non-verification submit requirements and
posts an explicit waiver message without inventing a positive Verified vote.
Emergency merging does not bypass repository permissions or branch protections.

A failed mandatory provenance node stops the emergency request. A stale target
CAS can reconstruct/retry the same authorized source revisions. A changed source
revision invalidates the request. Drain accepts no new emergency work.

Control writes use a private temporary file, fsync, rename, and directory fsync.
Invalid state blocks startup; uncertain writes block publication while preserving
the intended control in memory. Operator and policy audit entries are bounded by
count and bytes. Emergency records retain actor/reason and waived statuses in run
history. Admin routes require trusted ingress; a supplied actor string is audit
context, not authentication. GitHub comment authorization uses fresh repository
writer permissions.

## Infrastructure suspension

An optional per-target breaker counts infrastructure failures across distinct
revisions within a configured window. It never counts ordinary failing test exits
or model-inferred infrastructure diagnoses. Suspension and backoff persist.
Dispatch/publication stop while suspended; recovery probes use one change and
bounded exponential backoff. Infrastructure-error parks can supply a probe.
A completed probe without infrastructure errors clears suspension; it may
still reject the change for ordinary failing checks. Recovery never clears a manual pause.
