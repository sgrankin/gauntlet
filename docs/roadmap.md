# Feature implementation

The requested incident, recovery, investigation, and policy features are now
implemented. Stack preview and individual check waivers were removed from scope:
GitHub visualizes stacks, and emergency merging skips validation as a whole.

- Persistent per-target/global pause and resume, dashboard/admin API/CLI controls,
  urgency with starvation protection, and independent pause/check overrides.
- Revision-bound emergency prefix selection beside failed checks; active work is
  cancelled and reconstructed. Waivers remain explicit in history and statuses.
- GitHub urgent/emergency commands, durable request bindings, admission/failure
  feedback, stale-revision suppression, and eventual landing links.
- Optional persistent infrastructure breaker, bounded backoff, and recovery probes
  that preserve manual pauses.
- Codex-only failure inference with both authentication modes, optional bounded
  read-only Git/history tools, retained evidence, and a run-wide retry budget.
- Failure fingerprints, observed rerun outcomes, model failure kinds and suspects,
  dashboard history, and smaller-prefix batch recovery confirmed by real checks.
- Always-on named Rego decisions for commands, GitHub readiness, execution,
  deployment, and retries; custom extension/replacement, trusted principals,
  fresh batch validation, shared diagnostics, audit, and local policy fixtures.

Configuration and contracts are documented in [config.md](config.md),
[incident controls](design/incident-controls.md),
[failure investigation](design/failure-review.md), and [policy](design/policy.md).

Model diagnoses remain hypotheses. There is no framework-specific test-case
parser, calibrated confidence estimate, or proof of batch interaction attribution.
GitHub retains its documented close-with-landing-link completion behavior.
