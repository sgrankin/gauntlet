# Feature backlog

Requested October 3, 2026. This is the work queue, not a description of shipped
features. Completed work belongs in the configuration and API references.

## Admission and incident controls

- **Admission feedback and stack preview.** Show every requested member,
  prerequisite, readiness gate, and blocked reason consistently in PR feedback,
  the dashboard, and CLI. A dry-run must not post a merge request or enqueue work.
- **Failure feedback on GitHub.** Post check failures without requiring model
  analysis. Include the tested revision, failed check, useful output, retry state,
  and log link. Update durable, deduplicated feedback for the relevant request/run;
  stale runs must not overwrite a newer revision's status. Batch failures must
  distinguish an observed failure from suspected attribution. Retain the landing
  SHA/link when the change eventually lands, particularly while GitHub completion
  uses close-with-comment rather than native merged-state association.
- **Pause/resume.** Add per-target controls through the dashboard, CLI, and admin
  API, with a prominent incident button. Pausing must stop target publication,
  including already-green runs; distinguish this from shutdown drain. Preserve
  pause state across restarts and show actor, reason, and time. Decide explicitly
  whether incident pause cancels running verification or retains its results.
- **Hotfix priority.** Support an explicit `merge urgent` modifier and a stack
  equivalent. Preserve explicit stack authorization: priority never silently
  requests prerequisites. An urgent stack retains dependency order and all normal
  landing gates. Specify whether existing work is preempted and how normal work
  avoids starvation. Record who requested urgency and why.
- **Incident override.** Requested spelling: `merge urgent override`. The scope
  is pending clarification: permission to pass a pause, or an additional ability
  to waive named review-policy requirements. Do not implement an unrestricted
  bypass. Use explicit operator authorization, an audit reason, and a narrow scope.
  Verification and landing the exact tested commit remain mandatory.
- **Infrastructure circuit breaker.** Recognize shared builder/service failures
  across unrelated changes. Stop dispatch, surface the common cause, and probe
  recovery with bounded backoff rather than parking each candidate or repeatedly
  invalidating speculative work. Distinguish automatic suspension from a manual
  incident pause; recovery must not clear an operator's pause.

## Failure investigation and recovery

- **Read-only tools for model failure review.** Both existing providers currently
  classify supplied logs without tools. Add an opt-in mode with bounded tools:
  read a file from the exact tested tree, list/search tree paths, inspect candidate
  and batch diffs, and retrieve relevant previous failures and retry outcomes.
  Restrict reads to the chosen repository objects and approved history; do not
  expose the host filesystem, daemon config, credentials, network, or arbitrary
  command execution. Bound calls, bytes, tokens, duration, and retries. Preserve
  provider parity, cancellation, and credential isolation. Record consulted
  revisions and evidence. Repository text and logs are evidence, not instructions.
- **Model-assisted batch attribution.** Supply original candidate identities,
  individual deltas, stack dependencies, the batch base/tested tip, and failed
  check evidence. Return a set of suspected changes, evidence references,
  confidence, and an explicit unknown option. Interactions may implicate multiple
  changes. Use hypotheses to choose dependency-valid prefix/split reruns; do not
  declare a culprit, park a change, or skip required verification solely on model
  confidence. Confirm attribution through actual checks. No subgroup inherits the
  failed batch's verdict, and every landing must have its own exact tested tip.
- **Failure and flake history.** Record observed failure fingerprints, optional
  framework test identities, retry outcomes, and classifier decisions. Model
  descriptions help cluster and explain failures, but remain hypotheses. Report
  recurrence and actual rerun success rates separately from self-reported model
  confidence. Keep commands framework-neutral; optionally ingest standard test
  reports when available. Bound retries across a run as well as per check.
- **PR investigation feedback.** Extend ordinary failure feedback with model
  explanations, cited source/diff evidence, suspected changes, and confirmation
  results. Label uncertain conclusions clearly. Keep the failure-reporting path
  useful when the model is disabled, unavailable, or abstains.

## Advanced policy

- **Optional embedded Rego.** Support mutually exclusive inline `rego` KDL raw
  strings and `rego-file`, compiled and validated before activation. Keep the
  current simple configuration available and define how it relates to advanced
  policy. The policy is operator-owned; a candidate cannot weaken its own gates.
- **Stable policy facts and decisions.** Define a versioned input schema for
  requester permissions, current reviews/team membership, conversations, check
  results/producers, paths, branch, and stack. Missing facts and evaluation errors
  block landing. Return named requirements and actionable reasons, not just a
  boolean. Disable evaluator network and external capabilities and bound execution.
- **Policy audit and preview.** Store policy version and decisions, re-evaluate
  using fresh facts before landing, and provide local policy tests/dry-run tooling.
  Named policy overrides, if authorized, require their own permission and reason.

## Implementation order

Start with ordinary failure/admission feedback and stack preview, followed by
incident pause/resume, urgent priority, and the infrastructure circuit breaker.
Read-only model tools and failure history provide the evidence needed for batch
attribution. Advanced Rego remains a separate feature so policy-language work
does not hold up those controls. The override scope remains an open decision.
