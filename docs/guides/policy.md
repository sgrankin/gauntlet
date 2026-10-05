# Writing policy

## Operator policy

Shipped Rego controls GitHub ownership, command authorization, readiness, execution, deployment,
and retry decisions. Custom rules can extend or replace each decision. Inline and file policies are
mutually exclusive. Files resolve beside the daemon config. For example, require
an approval from the security team for sensitive paths, with no general approval
minimum, and restrict emergency check bypass to administrators:

```kdl
policy {
    timeout "100ms"
    teams "acme/security"
    rego r#"package gauntlet
import rego.v1

sensitive if {
    some path in input.paths
    startswith(path, "migrations/")
}
security_review if {
    some user, review in input.forge.reviews
    review.state == "APPROVED"
    review.current
    input.forge.teams["acme/security"][user]
}
default review_ok := false
review_ok if not sensitive
review_ok if security_review

default override_ok := false
override_ok if not input.emergency.skip_checks
override_ok if input.forge.permission == "admin"

requirements := [
    {"name": "sensitive-path-review", "satisfied": review_ok, "reason": "Security approval is required for migrations"},
    {"name": "emergency-authority", "satisfied": override_ok, "reason": "Only an administrator may bypass checks"},
]
submission := {"allow": every_requirement, "requirements": requirements}
every_requirement := count([r | some r in requirements; not r.satisfied]) == 0
"#
}
```

Use `rego-file "admission.rego"` instead of `rego` to load a separate file.
The [policy contract](../reference/policy.md) defines version-1 facts and adapter
availability. Missing facts/decisions and evaluation errors deny. Network and
nondeterministic capabilities are disabled. Policy compiles at startup and under
`gauntlet validate`; local fixtures use `gauntlet policy-check -config ... -input ...`.
Named decisions and hashes are audited; policy runs again with fresh facts before
publication. `extend "submission"` adds requirements to the shipped decision (the
default when no decision list is given). `replace "command" "submission"` makes
those named custom rules authoritative, including authorization and review
requirements. Every listed entry point must return a decision; there is no fallback
when a custom rule is undefined. Signing, exact revisions, configured receipt
provenance, and CAS remain code invariants.

Other entry points are `scope`, `execution`, `deployment`, and `retry`. Use
`gauntlet policy-check -decision command -config ... -input ...` to test a named
decision with local facts. `@gauntlet check` reports GitHub command authorization
and current PR readiness without requesting a merge.

## Choose which PRs Gauntlet manages

Use `scope` for ownership rather than readiness. For example, manage PRs targeting
configured queue branches except those labeled `external-merge`:

```kdl
policy {
    extend "scope"
    rego r#"package gauntlet
import rego.v1

external_merge := "external-merge" in input.forge.labels
scope := {
    "allow": true,
    "requirements": [{
        "name": "queue-owned",
        "satisfied": not_external,
        "reason": "PRs labeled external-merge belong to another workflow",
    }],
}
not_external := external_merge == false
"#
}
```

The shipped scope decision still requires a configured target. Managed open PRs
receive instructions even while draft or failing checks; excluded PRs receive
no instructions and cannot enter this queue. Removing ownership before landing
stops publication, including when the PR is a stack prerequisite.

Scope facts include labels, author, title/body, repositories, base ref, and
complete changed paths. They omit reviews, checks, conversation state, and
requester identity. Put authorization in `command` and readiness in `submission`.
When combining policies, list every custom entry point in `extend` or `replace`
and define each decision in the same policy source.
