# Failure decisions and in-place retries

An operator may enable `failure-review` for named, repeatable checks. A failed
command remains an in-flight check while its worker requests a decision. The
model chooses one of `retry`, `abort`, or `abstain`, with confidence in that
choice and a short evidence-based reason. It cannot return commands, edit the
trial, approve a merge, or change policy.

Gauntlet retries only `retry` decisions at or above `min-confidence`, within
`max-retries`. Everything else retains the failed check. A rerun must actually
pass; reporting `skipped` cannot clear an earlier failure. API refusal, malformed
output, timeout, and provider errors also retain the failure. Infrastructure
errors keep their existing handling rather than being reclassified as flakes.
The ordinary failure/bubble/batch-fallback path runs after this bounded review.

The retry happens inside the original worker. Its run ID, tested SHA, executor
profile, workspace, candidate-built image, service leases, and execution slot
remain fixed. Successful sibling checks are not repeated. Speculated successors
can complete while the failing predecessor is being reviewed; a successful retry
leaves their tested chain intact. A genuine target move, cancellation, or another
check's terminal failure still invalidates work normally. Classification uses the
worker's cancellation context and a separate per-decision timeout.

This deliberately retries the entire named check command, not a framework-specific
test case. Commands are opaque to Gauntlet. The operator must choose checks safe
to repeat in their existing workspace and against their existing services; shared
workspace mutations are not rolled back, and isolated mode retains this node's
original private workspace. Hooks, deployment commands, image builds, and receipts
are excluded. An operator can split expensive suites into independently retryable
checks without teaching the queue their test framework.

Original failure output, decisions, and rerun output are retained in the check's
history output (up to 12 KiB per attempt and 64 KiB overall). Full attempt logs are
kept separately and concatenated as zstd frames into the original log path, so the
existing dashboard full-log link includes all attempts. Losing supplementary log
writes does not change the verdict. CPU times sum across attempts, peak RSS takes
the maximum, and check duration includes classification and retry time.

## Providers and decision-model inspiration

Both API-key and ChatGPT access-token authentication use Codex CLI. Each call
uses an empty temporary workspace/home, no user configuration or rules, shell
tools disabled, and a read-only sandbox. API credentials are written only to
that private temporary Codex home; access tokens are passed in its isolated
environment. There is no separate direct-HTTP model adapter.

The evidence includes the check name, command, target, tested SHA, and a bounded
output tail. It excludes service environment, credential values, and workspace
paths. Repository output is untrusted evidence, explicitly distinguished from
instructions. Enabling the feature authorizes sending the selected checks' output
to the configured provider; operators should avoid secrets in test logs.

[Jev](https://typesafe.ai/blog/introducing-system-one-models-and-jev) and
[Clef](https://blog.cloudflare.com/clef-decision-models/) motivate the constrained
choice and abstention interface. Their probability calibration is a separate
property: a general model's requested confidence is **self-reported, not a
calibrated probability of correctness or of a rerun passing**. The threshold is
an operational heuristic. Decisions and resulting test outcomes provide evidence
for evaluating it. A future decision-model adapter can implement `flaky.Classifier`
without changing queue semantics; there is no assumed or invented forthcoming
OpenAI decision endpoint in this implementation.
