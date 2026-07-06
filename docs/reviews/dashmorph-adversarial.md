# Adversarial review — dashboard fetch+morph change

Scope: `/tmp/claude/review/dash-morph.diff` (DESIGN.md, dashboard_test.go, server.go,
static/idiomorph.min.js, templates/base.html, templates.go). Verified by reading the
templates, building, running the package tests, syntax-checking the emitted JS with
`node --check`, byte-diffing the vendored library against upstream, and driving idiomorph
in a real browser (Playwright) to confirm two load-bearing behavioral claims.

---

## BUG 1 — DESIGN.md + base.html claim "open `<details>` survive a refresh tick" is false

**Where:** `DESIGN.md` new ledger row ("…and any open `<details>` (a run's captured check
output) survive a refresh tick instead of getting blown away every 5s"); mirrored in
`internal/dashboard/templates/base.html:196-202` ("open-`<details>`…survive a tick").

**Why it's wrong — two independent reasons, both verified:**

1. **The pages that morph have no `<details>` at all.** Only `/` (index.html) and
   `/t/{target}` (target.html) set `Refresh` and run the poller. I read both in full:
   neither contains a single `<details>` or any `id=` attribute. The `<details>` elements
   the row points at ("a run's captured check output") live only in `run.html`
   (`internal/dashboard/templates/run.html:31,66`) — i.e. `/run/{id}`, which the change
   itself documents as a static page that **never sets `Refresh`** and never morphs. So the
   cited artifact lives exclusively on a page where the described 5s refresh does not happen,
   and is absent from every page where it does. The "any open `<details>` … survive"
   clause describes a situation that cannot occur.

2. **Even if a `<details>` were on a morphing page, idiomorph strips the user-set `open`.**
   Idiomorph *syncs* attributes from the fetched HTML onto the live element (see the
   attribute loop in the vendored source: for each old attribute not present on the new
   node it calls `removeAttribute`). The server always renders these collapsed
   (`<details …{{if .Open}} open{{end}}>` — `.Open` is server-side state for
   failed/errored checks, never a reflection of what the viewer toggled). A viewer opening a
   `<details>` sets the `open` content attribute in the live DOM; the next morph, diffing
   against server HTML that lacks `open`, removes it and collapses the element.

   Verified in a real browser (Chromium via Playwright): opened a `<details>`, morphed
   `document.body` against a body with the same details rendered closed →
   **`before=true after=false`** (the `open` attribute was stripped). The change's authors
   already know attribute-stripping happens — it is the entire stated reason
   `applyTimeTooltips()` must re-run after every morph (base.html:149-154: idiomorph "syncs
   attributes … a client-set title isn't in that fetched HTML, so it gets stripped on each
   morph"). `open` is exactly the same kind of client-set attribute, and nothing puts it
   back.

**Failure scenario (latent, not today):** a maintainer reads the DESIGN ledger, believes
open-details survive morphs, and adds a `<details>` to `target.html` (e.g. an expandable
"in flight" check panel). It silently snaps shut every 5s for anyone watching. The design
doc actively points them at a rake.

**Fix:** correct the claim. Scroll position and text selection genuinely do survive an
in-place morph (that part is true and is the real win) — but drop the "open `<details>`"
example, or, if preserving open state across morphs is actually wanted on the run page,
note that it would require either rendering server-side open state authoritatively or a
`beforeAttributeUpdated` callback that refuses to clear `open`. As written the row
overstates what the feature delivers and the example is unreachable.

---

## BUG 2 — poller has no in-flight guard: slow responses morph stale over fresh

**Where:** `internal/dashboard/templates/base.html:206-223` — `setInterval(…, 5000)` with a
bare `fetch` inside; nothing tracks whether the previous tick's fetch is still outstanding.

**Failure scenario:** `setInterval` fires every 5s regardless of in-flight requests. If a
response takes >5s (daemon GC pause, a wedged snapshot, or — the realistic case, since the
dashboard binds to a configurable address and is reached over a network — a slow/lossy
link), two or more fetches are concurrently outstanding. Promise resolution order is not
request order, so an older (staler) response can resolve *after* a newer one and morph stale
state onto the fresh DOM. A wedged backend for N×5s also piles up N concurrent fetches.

**Severity:** low. It self-heals — the next in-order successful tick repaints the correct
state ~5s later — and is unreachable on a fast local link. But it is a real correctness gap
with a trivial fix, and the guard also caps request pile-up under a slow backend.

**Fix:** a one-line in-flight flag:
```js
var busy = false;
setInterval(function () {
  if (document.hidden || busy) return;
  if (!window.Idiomorph) { location.reload(); return; }
  busy = true;
  fetch(location.href, {credentials: 'same-origin'})
    .then(function (res) { return res.ok ? res.text() : null; })
    .then(function (html) { /* …morph… */ })
    .catch(function () {})
    .finally(function () { busy = false; });
}, 5000);
```
(`finally` is supported everywhere `fetch` is.)

---

## NIT 1 — `Content-Type: application/javascript` vs the WHATWG-preferred `text/javascript`

`server.go:87`. `application/javascript` is universally executed by browsers, so this is
purely cosmetic; noting only because the WHATWG MIME spec now lists `text/javascript` as the
canonical JS type. If changed, update the assertion in
`dashboard_test.go` (`TestStatic_ServesIdiomorph`) which pins the exact string. Not worth
changing on its own.

## NIT 2 — 24h `Cache-Control` with no cache-busting on `/static/idiomorph.min.js`

`server.go:88` sets `public, max-age=86400` on a fixed URL with no version/hash in the path
and no `ETag`/`Last-Modified`. Re-vendoring a new idiomorph (the comment's own stated change
vector) ships a new binary but leaves the URL identical, so clients keep the old asset for up
to 24h with no conditional revalidation. Low impact for a self-hosted daemon dashboard
(operators can hard-refresh, and idiomorph re-vendors are rare), but a hash-suffixed path or
an `ETag` would make an upgrade take effect immediately.

---

## Verified clean

- **Supply chain — vendored idiomorph is authentic.** The code line of
  `internal/dashboard/static/idiomorph.min.js` is **byte-identical** to upstream
  idiomorph 0.7.4 (`dist/idiomorph.min.js`, fetched from jsdelivr):
  SHA-256 `0eb0f881553bd509fe7a7e3064a1089141e089583c506c1217e885a2e608b4a4` on both.
  No `eval`, `new Function`, `fetch`, `XMLHttpRequest`, `WebSocket`, `sendBeacon`,
  `navigator.*`, `document.cookie`, `localStorage`, or dynamic `import()` anywhere in the
  library. The only URLs in the file are the three in the human-written header comment
  (GitHub repo, unpkg LICENSE, unpkg dist) — no runtime network references.

- **No script double-arm after morph (verified in-browser).** Both `<script>` tags sit in
  `<body>`, so each morph diffs them. Confirmed with Playwright: an inline body script that
  increments a global ran exactly once and stayed at `1` across two `Idiomorph.morph(
  document.body, …)` calls, while a real change (footer text) still applied. Idiomorph
  patches the identical, id-less script elements in place; the browser does not re-execute
  an in-place-morphed script, so `setInterval` is armed exactly once. The double-poller
  compounding failure does not occur. (Would only change if a future edit gave the inline
  script an `id` or made its text vary per render, forcing remove+re-add — worth keeping in
  mind, but not a bug today.)

- **Emitted inline JS is valid in both branches.** Extracted the `<script>` body and
  `node --check`ed it with `.Refresh` true (poller present) and false (tooltip-only) — both
  parse. No template *action* interpolates data into the `<script>` context (only the
  `{{if .Refresh}}`/`{{end}}` control actions, which emit constant text), so html/template's
  JS-context escaping has nothing to mangle; the package tests assert the exact substrings
  (`Idiomorph.morph(document.body, doc.body)`, `setInterval(function ()`) survive rendering,
  and they pass.

- **Mux route is correct and unambiguous.** `GET /static/idiomorph.min.js` (server.go:63) is
  an exact pattern; it does not conflict with `GET /{$}` (which matches only `/`). Under the
  outer mux (cmd/gauntlet/dashboard.go: `/mcp` exact + `/` → dashboard), the request falls to
  the `/` subtree and reaches the dashboard's exact route; `/mcp` is unaffected.
  `TestStatic_ServesIdiomorph` exercises the route and passes (200, JS content type, non-empty
  body containing `Idiomorph`, proving the `//go:embed static/*.js` actually picked up the
  file).

- **`<head>` morph scope is fine.** The morph is body-only; nothing per-render lives in
  `<head>` except `<title>`, which is static per URL ("… · gauntlet" with a fixed page title
  for `/` and the target name for `/t/{target}`), so not updating it across ticks is correct.
  The footer "generated" timestamp is in `<body>` and does update.

- **`applyTimeTooltips()` re-run after morph is correct and idempotent.** It reselects all
  `time[datetime]` and reassigns `title`; safe to call repeatedly, and it correctly
  compensates for idiomorph stripping the client-set `title` (the same attribute-sync
  mechanism as BUG 1's `open`, but here it *is* restored).

- **`noscript` fallback / no bare meta-refresh.** base.html:7 wraps the old
  `<meta http-equiv="refresh" content="5">` in `<noscript>`; it fires only with JS disabled,
  so no double-refresh when JS is on. The reload fallback (`if (!window.Idiomorph)
  location.reload()`) covers the JS-on-but-idiomorph-failed case and can't false-positive:
  the `<script src>` is a normal blocking script before the inline one, so `window.Idiomorph`
  is defined by the time the interval first fires 5s later. `TestRefreshPages_
  CarryFetchMorphPolling` and `TestNonRefreshPages_NoPolling` pin the noscript-wrapped tag,
  the idiomorph `<script src>`, the poller, and their absence on `/run/{id}`.

- **Tests are meaningful, not tautological.** `TestStatic_ServesIdiomorph` checks the body
  is non-empty and contains `Idiomorph` (catches an empty/broken embed, not just route
  existence). `TestNonRefreshPages_NoPolling` asserts `applyTimeTooltips` is still present on
  a non-refresh page while the polling machinery is absent (guards against gating the tooltip
  code by mistake). Build clean, full `go test ./internal/dashboard/...` passes.

- **DESIGN.md row format** matches the surrounding `| **KEPT** | … | … |` table structure and
  parses as a table row. (Its *statement* is the subject of BUG 1.)
