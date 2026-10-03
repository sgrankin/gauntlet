# Operator admission policy

Optional embedded Rego adds requirements to the simple forge gates. It does not
replace requester authorization, source identity, approvals/conversations, signing,
conflict checks, or target CAS. Only explicit emergency check bypass affects test
requirements. Candidate-owned check specs cannot supply or change this policy.

Use `policy { rego r#"..."# }` or `rego-file "admission.rego"`, exclusively.
Relative files resolve beside the operator config. Policy compiles before queue
activation; `gauntlet validate -config ...` also compiles it. Evaluation defaults
to 100 ms (maximum one second). Network, runtime, and nondeterministic builtins
are unavailable; facts collection is bounded separately. Missing decisions,
malformed output, missing required facts, and errors deny.

The query is `data.gauntlet.decision`. Return `allow` and a `requirements` array
of objects with `name`, `satisfied`, and `reason`. Any unsatisfied requirement
makes the result a denial even if `allow` is true. Use explicit named reasons for
all gates that an operator or contributor might need to address.

## Input schema version 1

| Field | Meaning |
| --- | --- |
| `schema_version` | `1` |
| `phase` | `admission` or `landing` |
| `target`, `branch`, `base_sha` | Queue target, real branch, and run base |
| `candidate` | `ref`, `sha`, `version`, `source`, `source_base`, `depends_on`, `requester`, `urgent` |
| `stack` | Requested predecessor chain at admission; actual run members at landing |
| `checks` | Gauntlet check names and statuses; empty at admission |
| `emergency` | Independent `skip_checks` and `override_pause` booleans |
| `paths`, `paths_available` | Changed paths and completeness; renames include old paths |
| `forge` | Fresh adapter facts, or null for Git-ref intake |

GitHub facts contain `kind`, PR number, head/state/draft, requester/request ID,
permission, latest substantive reviews keyed by login (`state`, `commit`, `current`),
conversation resolution, checks with producer identity, paths, and explicitly
configured teams (`org/slug` → login → membership). Incomplete file lists fail
closed. Team membership lookups require suitable organization permissions; a
lookup failure denies, rather than assuming membership.

Gerrit facts contain change number, current head/state/work-in-progress,
submit requirements, and paths. Git-ref intake supplies immutable path facts but
no invented forge identity. Policies should branch on `candidate.source` when
adapters have different facts. Requiring a missing field denies normally.

Policy is evaluated at admission and again after fresh forge validation, before
the publication CAS. The last 500 decisions (subject to the total storage bound)
record policy hash, input hash, revision, phase, requirements, and errors in the
control file. This stores decisions, not credentials or full review payloads.

`gauntlet policy-check -config gauntlet.kdl -input facts.json` evaluates a local
fixture without forge writes or queue admission. It prints the policy hash and
named decision and exits unsuccessfully on denial. Keep policy fixtures alongside
operator configuration; local previews cannot replace fresh landing evaluation.
