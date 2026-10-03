# Receipts

A `receipt` node produces a bounded provenance payload after its declared
prerequisites. With operator `receipt-notes` enabled, exactly one receipt is
required and must publish before the tested tip lands.

```kdl
receipt "deployment" {
    command "./ci/write-candidate-receipt"
    executor "host"
    after "unit" "lint" "artifacts"
}
```

`command`, `executor`, `image`, and `after` mean exactly what they mean on
a check: the same executor-profile selection, the same candidate-built
`image` (with its implicit `after` on the image build), and `after` edges
against check names, validated the same way (unknown names, duplicates —
spec errors). **At most one** `receipt` node per spec — a second is a
parse error, not last-wins; the name is provenance (history, logs, graph
diagnostics), never a selector.

**A sink, not a barrier.** A receipt's name lives outside the check
namespace: `after` only ever resolves against declared *checks*, so no
check can name a receipt in its own `after` — it gets the ordinary
unknown-name error, exactly as if it had typo'd a check name. Nothing can
depend on a receipt in turn; it is scheduled as a `receipt:<name>` node
(prefix reserved, mirroring `image:`). But sink does not mean "runs
last": only its **declared** `after` edges (plus the implicit edge on its
`image`, if any) gate its readiness, so under `max-parallel > 1` it may
run concurrently with checks it doesn't name. Declare an edge on every
check whose outcome the receipt's contents depend on — the example above
names all three artifact producers. What "runs last" actually protects is
protected elsewhere: publication happens only once the *entire* graph is
green, so an unnamed check failing still prevents the note and the
landing.

**A different result-file protocol, not the check protocol.** The command
gets `GAUNTLET_RECEIPT_RESULT_FILE` *instead of* `GAUNTLET_RESULT_FILE` —
the same "distinct protocol, never conflated" contract as
`GAUNTLET_IMAGE_RESULT_FILE`. A receipt has no `skipped` verdict: non-zero
exit is red regardless of the file's contents, and exit 0 requires the
file to contain non-empty bytes within the operator's configured cap
(`receipt-notes { max-bytes ... }` in daemon config — see
[daemon configuration](daemon.md)). An empty, unreadable, or oversized result on a
zero exit is the receipt node's own red row — one root cause, not a
publication failure discovered later.

**The payload is opaque.** Gauntlet never parses, schemas, or interprets
the bytes your command writes — no deployment-manifest format, no JSON
merge strategy, no artifact graph. Your command owns constructing and
validating its own payload; gauntlet's whole job is capturing it exactly
and publishing it exactly, unmodified, as the note's content.

**The producer never sees the daemon's own credentials.** A `receipt`
node is candidate code like any check, so it runs under the same
config-named operator-secret stripping described in ["Check environment
reference"](environment.md#check-environment-reference) below — publication itself is
the *daemon's* job, using its own in-process authenticated transport, not
something the producer command does or needs credentials for.

**Policy handshake.** A `receipt` node only ever runs under a daemon
configured with a `receipt-notes` policy (`github { receipt-notes { ... }
}` — [daemon configuration](daemon.md)) for the target it lands against. Both
mismatch directions are rejected at spec load, loudly, before any command
starts:

- the daemon requires a receipt but the spec declares none:
  `this daemon requires a receipt (receipt-notes is configured) but the check spec declares none`
- the spec declares a receipt but the daemon has no policy for it:
  `check spec declares receipt "<name>" but this daemon has no receipt-notes policy`

Both directions reject rather than silently diverging on purpose: a
`receipt` declaration is a correctness claim ("this run has a durable
handoff before landing"), and running the producer while quietly
discarding the handoff would make one repository spec mean different
things on different daemons — worse than the rollout inconvenience of
failing closed. This also shapes rollout order: upgrade the daemon, turn
on `receipt-notes`, *then* land the candidate that adds the `receipt`
node — a candidate lacking it fails closed during that window, same as an
undeclared `needs` service.

Publication itself — when it runs, what it publishes onto, how a
deployment consumer reads it back — is a daemon-config and operations
concern; see [daemon configuration](daemon.md)'s `receipt-notes` reference and
deploy.md's ["Receipt read
path"](../guides/receipts.md#receipt-read-path-consuming-pre-land-receipts).
