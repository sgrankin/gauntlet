# Validating

## Self-checking your spec

`gauntlet validate -checks .gauntlet.kdl` parses and validates a check spec
with no daemon, no network, and no side effects — the same
`config.ParseChecks` the daemon itself runs against every trial tree. A repo
can declare a check that runs it against its own spec, so an edit that
breaks the spec (a typo'd `after`, a dependency cycle, a duplicate
`workspace`) fails its own merge trial instead of silently landing a broken
spec for the next candidate to trip over:

```kdl
check "validate-spec" {
    command "gauntlet" "validate" "-checks" ".gauntlet.kdl"
}
```

Without `-config`, only the spec's own internal validity is checked.
Cross-file properties — whether an `executor` name it references actually
exists, whether an `image` runs on a container-kind profile, whether
`service`/`needs` are usable at all, whether a `receipt` node is required
(or forbidden) by the daemon's `receipt-notes` policy — depend on the
daemon's config and can't be checked from the spec alone; run `gauntlet
validate -config gauntlet.kdl -checks .gauntlet.kdl` (with the operator's
real config) to catch those too. See `gauntlet validate -h` for the full
set of modes.

## Preflight: `gauntlet doctor`

Before starting the daemon on a new box (or after changing config, secrets,
or the host itself), run `gauntlet doctor -config /etc/gauntlet/gauntlet.kdl
-state /var/lib/gauntlet/state` — a host preflight covering exactly what
the configured host needs: Git 2.40 or newer,
`-state` existing/writable (plus an existing history database's schema
compatibility), GitHub authentication (a private-key-file's permissions and
a real token mint in `auth "app"` mode, or just the token-env var in
static-PAT mode), a read-only reachability check against the configured
remote, each configured executor profile's container runtime, and the
dashboard bind address. It prints one `PASS`/`WARN`/`FAIL` line per check,
only checks what the given config actually uses, and exits nonzero only on
a `FAIL` — a `WARN` (an already-in-use dashboard port, a container image
not yet pulled locally) is advisory. It is read-only except for one
deliberate exception: `auth "app"` mode mints a real, short-lived
installation token to prove the credential actually works end to end.
