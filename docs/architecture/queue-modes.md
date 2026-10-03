# Queue modes

All modes build per-submission commit links and publish the exact tested tip.
With the default squash landing:

```text
actual target → A → B → C
```

Each link's parent is its predecessor. A candidate cannot land ahead of a
required predecessor. See [target configuration](../reference/targets.md) for
knobs and bounds.

| Mode | Candidates per suite | In-flight runs | Main tradeoff |
|---|---|---|---|
| Serial | 1 | 1 | Straightforward attribution; no overlap. |
| Batch | Up to `max-batch` | 1 | Fewer suites; failure needs attribution. |
| Speculate | 1 | Up to `window` | Overlapping suites; invalidation can discard suffix work. |

## Batch boundaries and recovery

A batch tests its combined tip once and lands its members with one target push.
Each member receives its own run record and provenance; the shared batch ID
joins them. Check statistics count the suite once rather than once per member.
Post-land hooks run for each landed member against that member's commit.

A check-spec change ends the batch after that member, preventing later changes
from replacing its validation definition. A conflict or infrastructure failure
while building a chain stops formation at that member.

A red batch does not prove which member failed. Serial recovery retests members
individually. Bisect recovery tests smaller dependency-valid prefixes. Model
suspects may guide the next prefix; only real verification supports landing or
parking a member. Cancelling a member parks that member and requeues siblings.

## Speculation and invalidation

Every run tests the tip it would produce if its predecessors land. Only the
head can publish; its expected base must equal the actual target tip. Once it
lands, the next run's predicted base becomes actual, allowing a green prefix to
publish without rebuilding.

A failure or conflict on a predicted base does not prove that a candidate fails
against reality. It is requeued, with its dependent suffix invalidated. A
real-base failure parks the candidate. Source movement, review-version changes,
or an external target update also invalidate affected work.

The window bounds runs, not command concurrency. Repo `max-parallel` and the
operator's `max-executions` control execution demand.
