# Adversarial review: dash-polish.diff

Reviewed the full diff plus surrounding code in `internal/dashboard`, `internal/queue`,
`internal/history`, `internal/mcp`, and `cmd/gauntlet`. `go build ./...` and
`go test ./internal/dashboard/... ./internal/queue/... ./internal/history/... ./cmd/gauntlet/... ./internal/mcp/...`
all pass.

## Findings (most severe first)

### 1. BUG — parked outcome tag links to a 404 `/run/` page when history is disabled
`internal/dashboard/templates/target.html:76`

The new link is gated only on `{{if .RunID}}`, not on whether the dashboard has a
history store. A **live** park always populates `RunID` — `park()` in the queue daemon
sets `parkEntry.RunID = m.rec.RunID` unconditionally (`internal/queue/reconcile.go:1692`),
independent of the dashboard's store — so `parkedView.RunID` is non-empty even when the
daemon runs with history turned off. But `handleRun` short-circuits to `http.NotFound`
when `d.store == nil` (`internal/dashboard/server.go:239`).

Failure scenario: run gauntlet with history disabled, let a candidate get rejected/parked,
open `/t/{target}`. The Parked table's outcome tag is now an `<a href="/run/...">` that
404s on click. Before this change it was a plain `<span>` — so this is a new, user-visible
dead link in a supported mode (the template has an explicit "history disabled" branch at
`target.html:115`, and `RecentRuns` is correctly gated on `.StoreEnabled` at `target.html:103`).

The `parkedView` doc comment (`server.go` ~1183) reasons only about the *boot-seed*
history-disabled case leaving `RunID` empty; it misses that a **live** park during a
history-disabled run still sets a `RunID`. That gap is exactly what produces the dead link.

Fix: gate the link on the store too, mirroring the `RecentRuns` section:
```
{{if and .RunID $.StoreEnabled}}<a href="/run/{{.RunID}}" class="tag {{.Outcome.Class}}">{{.Outcome.Word}}</a>{{else}}<span class="tag {{.Outcome.Class}}">{{.Outcome.Word}}</span>{{end}}
```
`$.StoreEnabled` is in scope on `targetData` (set at `server.go:199`, false when `d.store == nil`).

### 2. NIT — inline JS has no `Intl.RelativeTimeFormat` guard; a throw loses the local-time tooltip too
`internal/dashboard/templates/base.html:142` (the `relative()` function)

`new Intl.RelativeTimeFormat(...)` is constructed unconditionally. On a browser without it
(pre-2020 Safari/older engines) it throws `TypeError`. The throw happens *inside* the
`forEach` callback, on the line `el.title = d.toLocaleString() + '\n' + relative(d)` — so
the more useful local-timezone tooltip (`toLocaleString()`, which needs no
`RelativeTimeFormat`) is lost as collateral, and the exception aborts the callback for every
remaining `<time>` element on the page. It will *not* "kill other scripts": base.html is the
only `<script>`, and browsers isolate exceptions per event handler, so page rendering is
unaffected — just no tooltips. Low real-world impact in 2026 (evergreen browsers all support
it), but the graceful-degradation intent isn't met.

Suggested fix: compute the relative part defensively so local time always lands, e.g.
`var rel = ''; try { rel = relative(d); } catch (e) {}` and only append when non-empty; or
guard once with `if (!('RelativeTimeFormat' in Intl)) { /* set toLocaleString-only titles */ }`.

### 3. NIT — `Intl.RelativeTimeFormat` re-constructed once per `<time>` element
`internal/dashboard/templates/base.html:142`

`relative(d)` builds a fresh `new Intl.RelativeTimeFormat(...)` on every call, i.e. once per
`<time>` element. Purely a minor efficiency point (constructing an `Intl` formatter isn't
free); hoist the formatter out of the loop. Not a correctness issue.

## Verified clean

- **template.HTML in attribute contexts** — checked every field that changed `string`→`template.HTML`
  against every render site across all six templates. Every one lands in element *content*,
  never an attribute:
  - `baseData.GeneratedAt` → `base.html:141` footer content only.
  - `ignoredRefView.At` → `index.html:41` `<td>` content.
  - `parkedView.At` → `target.html:76` `<td>` content.
  - `hookRunView.StartedAt` → `target.html:93` `<td>` content.
  - `runSummary.StartedAt` → `target.html:108` `<td>` content (index.html's RecentRuns render
    only chips, not StartedAt; the chip `title="{{.Title}}"` is the separate unchanged `string`
    field built by `chipTitle`, not from `formatTime`).
  - `runSummaryFull.StartedAt/EndedAt` → `run.html:22-23` `<td>` content.
  - `batchMemberView.StartedAt` → `batch.html:17` `<td>` content.
  - `checksData.Since` → `checks.html:2` `<p>` content. The suspected form-input `value="..."`
    does not exist; the `since` filter is a URL query param, not a rendered input. Clean.
  - grep for `title="{{...StartedAt|EndedAt|At|Since|GeneratedAt...}}"` across templates found
    nothing — the only `title=`/`value=` attributes bind unchanged `string` fields (`.Ref`,
    `.SHA`, `.Title`). No raw `<time>` markup can break or double-escape an attribute.

- **CSS `.check-row` / details double-border fix** — matches the actual markup: `run.html` uses
  both `<details class="check-row">` (with `<summary>` + `<pre class="output">` inside, lines
  35-38, 59-62) and plain `<div class="check-row">` (lines 40, 64). The outer box now owns
  padding+border-bottom once; an expanded `<details>` puts its border below the enclosed
  `<pre>`, and a plain `<div>` row is unchanged (it only ever matched `.check-row`). The
  disclosure-marker behavior is **identical to before**: the old combined selector already gave
  `summary { display: block }` (which suppresses the default `list-item` marker), and the new
  rule keeps `display: block` on summary — so the surviving `::marker` rule is no more/less
  effective than it was. No regression.

- **park() signature / run-ID semantics** — every call site passes the semantically-correct ID:
  - `command.go:223` cancelBatchMember → `m.rec.RunID` = the member's own `memberRunID(runID, i)`,
    not the batch head. Note the emitted *event* carries `r.runID` (head) while the park uses the
    member's `rec.RunID` — but this is correct: `history.Store.Emit` persists the run keyed on
    `ev.Record.RunID` (`store.go:250` → `writeRecord` uses `rec.RunID`, `store.go:273`), which
    equals the park's `m.rec.RunID`. So `/run/{memberRunID}` resolves.
  - `command.go:263` cancelWaiting → the synthesized `runID`, matching the emitted `Record.RunID`.
  - `reconcile.go:1098` rejectBatch → per-member `rec.RunID` (`memberRunID(runID, i)`).
  - `reconcile.go:1499` finishRun → `m.rec.RunID`.
  - `reconcile.go:1627/1649` rejectPreMerge/rejectRun → the same `runID` their emitted Record carries.
  - `finishBatchRed` (`reconcile.go:1125`) does **not** park at all (unknown-guilty → nothing
    parks), so there's no member/head confusion there.
  - `runs` are never pruned (`history/store.go:478` says so explicitly), so an enabled-history
    park link can't 404 from retention.

- **Boot seeding SQL** (`history/queries.go:434`) — adding `t.run_id` to the SELECT doesn't
  change the window function: `ROW_NUMBER() PARTITION BY candidate_ref ORDER BY started_at DESC,
  run_id DESC` already ordered on `run_id`; the outer `WHERE t.rn = 1` still selects the same
  winning row, and `run_id` is read from that same row (`t.`). The `retry_intents` LEFT JOIN and
  its `ri.at <= t.ended_at` suppression are untouched, so a park seeded from a retried/suppressed
  row still doesn't resurface. `Scan` order updated to match. Clean.

- **JSON compat** — `parkedStatus.RunID` added identically to both `dashboard/api.go:307` and
  `mcp/server.go:305` (`json:"runId,omitempty"`), both populated from `pe.RunID` — shapes stay in
  sync. `cmd/gauntlet status` decodes via `json.Unmarshal` into its own `statusAPIParked` struct
  (`status.go:122`) with no `DisallowUnknownFields`, so the new field is silently ignored — the CLI
  is **not** broken. (Its text render at `status.go:233` prints only ref/outcome/reason — it
  already omits `at` too, so not carrying `runId` is a pre-existing parity gap, not a regression;
  worth a separate parity note but out of scope for this diff.)

- **Relative-time unit logic** — the `units` walk always terminates (`'second'` matches
  unconditionally); past/future signs, sub-second (`round(0)`→"now"), and minute/hour/day
  boundaries all format sensibly. Month/year are calendar approximations (30/365 days) but that's
  expected for a tooltip. `isNaN(d.getTime())` correctly skips unparseable `datetime`; zero-time
  values render as bare `"-"` with no `<time datetime>` wrapper, so `querySelectorAll('time[datetime]')`
  never matches them. RFC3339 `...Z` strings parse in all engines.
