# Adversarial review — slack-v2 durable command ownership

Scope: the one commit in `/tmp/claude/slackv2.diff` (internal/slack/slack.go, slack_test.go, fake_test.go, README.md). Working copy has the diff applied.

Gates run:
- `go build ./...` — clean
- `go vet ./internal/slack/` — clean
- `go test -race -count=1 ./internal/slack/` — PASS (2.6s)

Verdict up front: the security model is sound (metadata is only ever trusted after a server-set-authorship check that is not spoofable), and the new tests genuinely drive the real socket entry point, so they would have caught the original "reaction after termination is ignored" bug. But there is **one load-bearing correctness risk that the fake papers over** — the same failure shape as the bug that shipped before — plus a couple of should-fix behavior/doc gaps.

---

## BLOCKER / TOP RISK

### 1. Bot-authorship check relies on `msg.User`, which real Slack may leave empty for bot-posted messages — the fake hard-codes it, so a green test suite proves nothing about the live path. (slack.go:340, :1172-1174; fake_test.go:221, :243-247)

`isOwnMessage` is the entire trust boundary for the post-termination path:

```go
func (s *Slack) isOwnMessage(msg goslack.Message) bool {
    return s.botUserID != "" && msg.User == s.botUserID
}
```

`botUserID` is set from `auth.test`'s `UserID` (slack.go:340). The problem: a message posted by a bot via `chat.postMessage` and read back through `conversations.history` is a **`subtype: "bot_message"`** object. Its `bot_id` (B…) is *always* populated; its top-level `user` (U…) field is **not reliably populated** for bot messages — depending on app/workspace configuration it can be empty, with identity carried only in `bot_id` / `bot_profile`. The `goslack.Message` struct (messages.go:65, :78, :86, :89) has all of `User`, `SubType`, `BotID`, `BotProfile` precisely because bot messages don't always carry `User`.

If `msg.User` comes back empty in production, `isOwnMessage` returns false for the daemon's *own* root, the metadata fallback silently ignores it, and **reaction-retry-after-termination is broken again** — the exact bug this commit exists to fix, one layer deeper.

The fake cannot expose this because it *chooses* to populate `user`:
- `handlePost` sets `user: fakeBotUserID` on every recorded message (fake_test.go:221)
- `handleAuthTest` returns that same id (fake_test.go:243-247)
- `injectForeignMessage` is the only path that varies `user`, and every test that seeds an "owned" message goes through `handlePost`, so the owned-message case is *always* `user==botUserID`.

So the suite asserts "if `user` matches, we trust it" — it never tests the shape a real bot message actually has (`bot_id` set, `user` possibly empty). This is structurally identical to the prior dishonest fake.

**Confidence:** high that the code only checks `User`; medium that real Slack leaves `User` empty here (it is config-dependent and I cannot confirm it from code alone — it needs a live check against the real workspace). Given the project's history of exactly this class of miss, treat it as blocking until verified live.

**Fix:** capture `resp.BotID` at Run start too, and accept a message as own if **either** `msg.User == botUserID` **or** `msg.BotID == botID` (bot_id is the field guaranteed present on bot-posted messages; another app's messages carry a *different* bot_id, so this does not weaken the spoofing check at all). Then add a fake path / test that models a bot message with `user==""` and `bot_id` set, and assert it still resolves. That test is the one that would actually prove the live path.

---

## SHOULD-FIX

### 2. `refRetry` can hijack a plain re-push of the same ref within the TTL; the doc comment claims this is prevented, but the rollback only covers the cmds-full case. (slack.go:1069-1090, :417-438)

`mintCommand` records `refRetry[{target,ref}]` before sending the retry command, and only rolls it back when the `s.cmds` send fails (buffer full). In the normal send-succeeds path the entry persists for `refRetryTTL` (1h). But a retry command is **not guaranteed to produce a trial-clean**: reacting `:recycle:` on a root whose ref is *not currently parked* (e.g. a ✅ landed root, or one already cleared) makes `applyRetry` a no-op (command.go:70-89) — no `EventTrialClean` is ever emitted for that retry. The `refRetry` entry then sits armed. If the same ref name is genuinely re-pushed within the hour, `postRoot` (slack.go:423-431) consumes the stale entry and routes the *new push* through `postRetryRoot`: it threads the fresh run under the **old** root and re-edits that old root's text to "⏳ retrying …", clobbering whatever it showed (e.g. the prior ✅).

The doc comment at slack.go:1066-1068 explicitly asserts "a dead entry can't hijack an unrelated future trial-clean — e.g. a plain re-push within the TTL," but the rollback it describes fires **only on the cmds-full branch**, not on the far more common "send succeeded, retry was a no-op" branch. So the comment overstates the guarantee.

Impact is presentation-only (the queue still runs and lands the push correctly; only the Slack rendering lands on the wrong, reused root), and it requires reacting `:recycle:` on a non-parked/landed root followed by a same-name re-push within 1h. Not catastrophic, but real, and the misleading comment will cost the next reader.

**Confidence:** high on the mechanism; the scenario is plausible-but-uncommon.

**Fix options:** either drop the eager `refRetry` write and only arm continuity when the retry actually clears a park (harder — the channel doesn't see park state), or at minimum correct the doc comment to state the true bound, and consider validating at consume time that the run being threaded is plausibly the retry's own (e.g. tie the entry to the SHA or a one-shot arming that a plain re-push wouldn't satisfy). At the very least, fix the comment so it stops claiming protection it doesn't provide.

### 3. `refRetry` is only swept when a *new* retry is minted — not opportunistically like `batchRecs`. (slack.go:1099-1107)

`recordRefRetryLocked` sweeps expired entries, but it is the *only* sweeper, and it runs only when another reaction-retry is minted. `batchRecs` by contrast is swept on every batch-terminal arrival (`collectStaleBatchesLocked`). So if reaction-retries are rare, a handful of never-consumed entries persist well past their TTL — indefinitely if no further retry is ever minted. Bounded by "distinct refs reacted-`:recycle:`-on within a window," so not an unbounded leak, and the field comment loosely acknowledges it — but it is weaker than the `batchRecs` bound the comment compares itself to. Low severity; worth a `Stats()`-style assertion or a note that this map, unlike the others, is not guaranteed to return to zero.

---

## NITS / OBSERVATIONS

### 4. The fake's `conversations.history` ignores the channel parameter, so a wrong-channel fetch would pass tests. (fake_test.go:268-305; slack.go:1121-1132)
`handleForeignReaction` fetches from `reaction.Item.Channel` (the fake injects `"C_FAKE"`, slack.go via sendReaction fake_test.go:430) while `s.channel` is `"C_TARGET"`. Real Slack would carry the message's real channel here and it would equal `s.channel`; the divergence is harmless in production (the ack correctly hard-codes `s.channel`, and a gauntlet root only ever exists in `s.channel`). But because the fake's `handleConversationsHistory` looks up purely by ts and never checks the channel form field, a regression that fetched the wrong channel would not be caught. Test-fidelity gap, not a product bug.

### 5. Coverage gaps in the new tests (none blocking, but named for honesty):
- **Malformed-but-owned metadata**: `handleForeignReaction` handles `target == ""` (slack.go:1151-1156) and a non-string `ref`, but no test drives an owned `gauntlet_run` message with a missing/empty/typed-wrong `target`. `injectForeignMessage` can't currently seed an *owned* (user==fakeBotUserID) message, so this branch is untested.
- **Empty `botUserID` fail-closed**: the auth.test-failed path (slack.go:337-341 → `isOwnMessage` never matches) is asserted only in prose, never exercised. A test that starts the channel with a failing auth.test and confirms a post-termination reaction is ignored would lock in the fail-closed contract.
- **Message-not-found** (`len(resp.Messages)==0`, slack.go:1137-1141): no test reacts on a ts unknown to `conversations.history`.

### 6. Recursion / self-reaction is correctly avoided (verified, not a finding). The `eyes`/`question` acks the bot adds do fire `reaction_added` back to the bot, but `reactionCommandKind` returns `ok=false` for both (slack.go:1039-1048), so they are ignored — no command loop. Good.

### 7. Contract nil-checks are all present (verified): `postCheckReply` nil-checks `ev.Check` (slack.go:512), `postHookFinished` nil-checks `ev.Check` (:713), terminal routing keys on `ev.Record != nil` (:391) so a terminal-kind event with a nil Record is safely ignored rather than dereferenced, and unknown `EventKind` falls through `handleOutbound`'s switch to a no-op (:389-402). Reaction on a non-`"message"` item is skipped before any fetch (:1010-1014).

### 8. Concurrency is clean (verified): `botUserID` is written once in `Run` **before** the `go` statements that start the reader goroutine, so the go-statement happens-before edge publishes it safely — no race despite the lock-free read in `isOwnMessage`. `now` is likewise write-once in `New`. All `runRoot`/`roots`/`refRetry`/`batchRecs` accesses are under `s.mu`. `-race` is green.

### 9. README is accurate: it now lists `reactions:write` and `channels:history` as required scopes (both genuinely needed: `reactions.add` and `conversations.history`), and documents the batch-root `❓` exception matching the code. One nit: `channels:history` covers public channels only; a private posting channel would need `groups:history`. Minor.

---

## Bottom line
The design and the tests are a real improvement and the security posture is correct. The one thing that would let this ship broken-in-production-but-green-in-CI is finding #1: the authorship check trusts a field (`msg.User`) that real bot messages may not populate, and the fake hard-codes that field so the suite can't tell. Verify against the live workspace (or switch to a `bot_id` match) before trusting the green checkmark — that is precisely the trap this codebase fell into last time.
