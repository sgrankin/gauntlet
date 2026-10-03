# Receipts

## Receipt read path: consuming pre-land receipts

When `github { receipt-notes { ... } }` is enabled (config.md's
[reference](../reference/daemon.md); the repo side is
`receipt` in [check reference](../reference/receipts.md#receipts)), the daemon publishes each
landing's receipt as a git note on the exact tested `MergeSHA`, before the
target moves. That note is not something a deployment system gets for
free.

**Notes refs are not fetched by default.** An ordinary `git fetch`/`git
clone` does not bring notes along; a consumer must explicitly name the
ref:

```sh
git fetch origin refs/notes/gauntlet/receipts:refs/notes/gauntlet/receipts
git notes --ref refs/notes/gauntlet/receipts show <landed-merge-sha>
```

`<landed-merge-sha>` is the merge commit gauntlet actually landed — the
one your deployment trigger (a push to the target, a landing webhook, a
poll of the branch tip) already hands you. Do this fetch every time you
need a receipt; a stale local copy of the notes ref is a stale view of
what's been published since.

**Validate before you act.** The daemon's job ends at "these exact bytes
are attached to this exact commit" — it never parses, schemas, or
verifies the payload. Your deployment code must: read the note, validate
the receipt bytes against whatever format your `receipt` command produces
(issue #13 deliberately leaves that opaque to gauntlet), and confirm the
artifacts it names still exist in their registry/object store — *before*
mutating anything. A missing note, an unparseable payload, or a
now-vanished artifact should stop the deployment, not proceed on faith.

**Trust boundary, stated honestly.** A git note does not cryptographically
bind to the commit it annotates — it's a separate object graph a
sufficiently-authorized repository writer can replace, exactly as a
writer to an artifact registry could replace an object there. This
feature's protection is layered, not cryptographic:

- an operator-owned notes namespace and a daemon-only write path (no
  repository command, including your own `receipt` producer, ever holds
  the credential that publishes);
- the normal publisher's no-overwrite/idempotency checks (same bytes:
  success; different bytes on the same SHA: fail closed — see
  config.md's `receipt-notes` reference);
- an auditable notes-ref history (every publish is a commit on that ref,
  same as any other git history);
- exact merge-SHA attachment, so a receipt is unambiguously about one
  specific tested tree; and
- repository-side receipt validation plus artifact-existence checks,
  which stay entirely in your control.

Payload signing or provider-side ref protection can be layered on top of
this later if a deployment's threat model needs protection from every
repository writer, not just from accidental or unauthorized publication
paths. Neither is required, or assumed, by the first implementation.
