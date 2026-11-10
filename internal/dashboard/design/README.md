# Dashboard design (block 3 draft)

First draft of the `backsight dashboard` UI for issue #115 (decision 0079 §8–§10). Block 5 translates it into Go `html/template`. The author approved it on 2026-10-30, including departures 1 ("3,600 chars"), 2 (the 9,000-character context limit) and 4 (Found tile watch while the Found axis is ok when the server is down) listed below.

- **Claude Design canvas**: https://claude.ai/artifact/FbCVvwY5S8pvPPp8nppE8E (private until shared from its Share menu)
- `tokens.css`: colours for both themes (ink, status, the three category sets, a one-hue sequential ramp, chart chrome), type scale on the system font stack, spacing, radii, layout sizes.
- `reference/dashboard.css`: the single stylesheet built on `tokens.css`. The canvas uses this same file, so the mockups and the references cannot drift apart.
- `reference/overview.html`, `reference/memory.html`: static HTML, no script. Open them straight from disk; they load the logos from `docs/assets/`.

`canvas/` holds read-only, byte-for-byte copies of the 26 canvas artboards (`*.dc.html`) the pages are built from, taken on 2026-11-01, the day block 5 started (refreshed the same day with the block 6 draft: 27 new `B6*` artboards and the three masters it changed, see "Block 6 additions"; refreshed again on 2026-11-06 with the block 8 draft: 19 new `B8*` artboards and the three masters it changed, see "Block 8 additions"), so the scenarios' wording and numbers can be read offline and reviewed in a diff. The canvas above stays the source: a change to the design is made there and these copies are refreshed from it, never edited here. They are mockups that need the canvas's runtime (`support.js`, `<dc-import>`, inline styles): nothing embeds, serves or links them.

## Rules for block 5 (from the review)

- **No inline styles.** The dashboard's CSP is `style-src 'self'`, which blocks every `style="…"` attribute. The references have none (`grep -c 'style="' reference/*.html` is 0). A value computed per item in Go must be an SVG attribute (`x`, `y`, `width`, `height`, `d`, `points`, `viewBox`) or one class from a finite set in `dashboard.css`, never a style. The canvas artboards still use inline styles for layout, which is fine there: they are mockups, not the page.
- **Copy buttons:** render every `.copy` with the `hidden` attribute; the one small copy script removes it. With scripts off, the command stays selectable text and no button that does nothing shows.
- **Logos:** the references load `../../../../docs/assets/logo-*.svg` only so they open from disk. Block 5 serves them at `/assets/<hashed>.svg` (embedded, no session needed, so the 401 page shows them too).
- **Status icons are `data:` SVG masks in `dashboard.css`.** The icon shape is a non-colour status cue, so the CSP must keep `data:` in `img-src` (decision 0079 §7 has `img-src 'self' data:`). If that changes, move the icons to `/assets/` files.
- **Footer version** (`v0.1.17-m7` in the mockups) is a sample: the page prints the running binary's version from `internal/buildinfo`.
- **The dark palette is written three times:** `tokens.css` under `@media (prefers-color-scheme: dark)`, `tokens.css` under `[data-theme="dark"]`, and, as a forced-colours simulation, `.fc-preview` in `dashboard.css` (mirroring its `@media (forced-colors: active)` block). Each place carries a "keep in sync" comment.
- **Sample names:** apart from `backsight`, project names are neutral samples (`project-a`, `project-b`, `project-c`, and `project-d-shared-frontend-tools` to show the 24-character ellipsis).

## What the canvas holds

| Row | Artboards |
|---|---|
| 1. Start and components | `Main`: index, category colours (both themes), status set, colour-blind table, number formats. `Components`: top bar (closed; Browse, Developer, project filter, preferences open; under 900 px with the Menu open and closed), scope line (ok, stale, catching up, server down), chips, every status word, deltas, ids and truncation, the verdict banner in every axis state, the summary line with and without prices, a tile in every status (ok, watch, attention, borderline, unreliable, too few, learning, developer mode, ⓘ open, not filtered), CLI disclosure open, an attention item with "why this matters" open, a turn-rail row, a sortable table with id chips and truncation, tabs in a card. `ChartsStates`: sparkline, line chart with faint n bars (table view open), line chart with a limit line, stacked bars (outcomes, with pending hatched; sessions sent, with failed labelled), stacked area, histogram, strip timeline, info badges, every empty and error state (fresh install, feedback off, server-only host, too few, server down, empty window, catching up, stale, project with no memories), and a forced-colours preview. Masters reused by every page: `TopBar`, `Banner`, `Tile`, `LineChart`, `StackBars`, `StackArea`, `Histogram`, `StripTimeline`. |
| 2. Overview, light, 1440 | New install (day 2), Healthy, Can't judge, Quality watch with a borderline tile, Latency watch, Server down on a laptop, Broken. A pink guide band marks the 900 px fold. |
| 3. Overview, dark, 1440 | The same seven. |
| 4. Developer mode and 390 px | Overview with developer mode on (light, dark; filtered to `backsight` so the "not filtered" chip shows), Overview at 390 px (light, dark; Latency watch scenario, so an attention item shows), Session page at 390 px (light, dark). |
| 5. Pages, light | Quality, Cost, Bank, Pipeline (shown in the Broken scenario), Session page with the turn rail, Memory page with the strip timeline, System. |
| 6. Pages, dark | The same seven. |
| 7. Light only | Projects, Sessions and Memories lists, Releases (per version, with covariates), Definitions, a Day page, 401, and one board with first build, update in progress and updated. |

Every page artboard is a fluid page (`expand: fill`): open one full-window to see it at your own width.

## Colour-blind check

Method: every category set was run through the dataviz skill's validator (`validate_palette.js`: Viénot/Brettel simulation of deuteranopia, protanopia and tritanopia, ΔE in OKLab × 100, **all pairs**, not only neighbours) against the card surface of each theme (#FFFFFF light, #161B22 dark). Target ΔE ≥ 8; 6–8 is allowed only with a second cue; normal-vision ΔE must be ≥ 15.

| Set | Theme | Colours | Worst CVD ΔE | Worst normal ΔE |
|---|---|---|---|---|
| Where memory went: used, shown then ignored, not declared, shown only | light | #2A78D6 #D9581E #48505A #8A949E | 10.7 (tritan) | 16.8 |
| same | dark | #3987E5 #D95926 #A3ACB6 #6B7480 | 12.1 (protan) | 15.5 |
| Kinds: summary, fact about you and your work, fact from a session | light | #7A4FD0 #138A72 #C76A1A | 10.0 (protan) | 22.2 |
| same | dark | #9085E9 #1FA58A #C97B2E | 8.9 (tritan) | 20.7 |
| Sessions sent: sent, queued, set aside, failed | light | #2A78D6 #8A949E #B07C00 #C4314F | 9.8 (deutan) | 15.3 |
| same | dark | #3987E5 #6B7480 #C99A1E #FF8A8A | 7.1 (tritan, set aside vs failed) | 15.5 |

- **Pending** is not a colour: it is a 45° hatch of the "shown only" ink, outlined, so it differs from every fill by texture.
- **Sessions sent, dark** sits in the 6–8 band for set aside vs failed under tritanopia; its second cues are the value label printed above every failed bar and the table view.
- On the strip timeline each outcome also has its own **shape** (circle, diamond, square, small dot, dashed ring), so it reads without colour at all.
- **Status colours** (ok, watch, attention) are *not* separable by hue under deuteranopia (validator ΔE 3.1 for watch vs attention) and are never relied on: every status carries its word and an icon of distinct outline (circle with a check, triangle, octagon with a cross, half-filled circle, dashed circle, circle with a question mark, hourglass).
- Some steps of the validator's aesthetic checks fail on purpose: the greys (not declared, shown only, queued) fail the chroma floor because they are neutral by meaning, and two dark steps sit above the lightness band, which makes them more visible, not less.

## Contrast (WCAG 2.x)

Text on the card surface / page background / inset surface; status text also on its own tint. Requirement: 4.5:1.

| Token | Light | ratios | Dark | ratios |
|---|---|---|---|---|
| `--fg` | #1F2328 | 15.80 / 14.48 / 13.84 | #E6EDF3 | 14.64 / 16.00 / 13.39 |
| `--fg-2` | #4A535D | 7.82 / 7.17 / 6.85 | #B8C1CB | 9.50 / 10.38 / 8.68 |
| `--fg-3` (muted) | #5C6670 | 5.85 / 5.36 / 5.12 | #8E98A2 | 5.90 / 6.45 / 5.40 |
| `--link` | #9C3560 | 6.79 / 6.23 / 5.95 | #EE93B5 | 7.81 / 8.53 / 7.14 |
| `--accent` | #B4436C | 5.30 / 4.86 / 4.64 | #E07A9F | 6.14 / 6.71 / 5.61 |
| `--ok` | #17733A | 5.92 / 5.42 / 5.18, tint 5.21 | #4AC25E | 7.55 / 8.26 / 6.91, tint 6.81 |
| `--watch` | #875800 | 6.13 / 5.62 / 5.37, tint 5.49 | #DDA640 | 7.91 / 8.65 / 7.23, tint 7.16 |
| `--bad` | #BC2530 | 6.09 / 5.59 / 5.34, tint 5.24 | #FF7B72 | 6.86 / 7.50 / 6.27, tint 6.19 |
| `--neutral` | #59636E | 6.11 / 5.60 / 5.36, tint 5.26 | #9AA4AE | 6.83 / 7.47 / 6.25, tint 5.72 |
| `--learn` | #0B62C4 | 5.90 / 5.41 / 5.17, tint 5.08 | #6CB2FF | 7.78 / 8.51 / 7.12, tint 6.93 |

Graphic contrast of category fills against the card surface (requirement 3:1):

| Fill | Light | Dark |
|---|---|---|
| used / sent | 4.42 | 4.75 |
| shown, then ignored | 3.91 | 4.45 |
| not declared | 8.17 | 7.52 |
| shown only / queued | 3.08 | 3.65 |
| summary | 5.45 | 5.53 |
| fact about you and your work | 4.28 | 5.61 |
| fact from a session | 3.81 | 5.24 |
| set aside after an error | 3.67 | 6.69 |
| failed | 5.39 | 7.62 |

The one-hue sequential steps (`--seq-1..3`, used only for splits that are not categories: session start vs prompts, server task types, empty reasons, churn) have one step under 3:1 in each theme (#9EC5F4 light, #184F95 dark); those charts always have a legend, a 2 px surface gap between segments and the table view.

## Where this draft departs from the issue, and why

1. **"3.6 k chars" in the scenario table is shown as "3,600 chars".** The number-format table says k only from 10,000; the tiles follow it. Tokens in the summary line (17 k) do use k.
2. **The context limit reads 9,000 characters.** `recall.session_budget_chars` defaults to 6,000 (docs/configuration.md), so "1.5 × budget" is 9,000.
3. **Severity `bad` is worded "attention".** The status words of §9 have no "bad"; watch keeps the triangle, attention gets the octagon.
4. **Server down: the Found tile says watch while the Found axis says ok.** The tile reports the metric of one window (9 %); the axis rule needs two windows, and the root cause belongs to Flowing, which names it once.
5. **ⓘ is a `<details>`** (summary with `aria-label`, popover inside), not a button with `aria-describedby`, so it opens with scripts off. Under 900 px the popover docks to the bottom of the screen.
6. **Responsive rules use a container query on `.page`** (same 900 px breakpoint). The layout is the same as with a media query, and the 390 px mockups reflow with no extra markup.
7. **Forced colours:** chrome takes system colours; chart fills keep their colours as data (`forced-color-adjust: none`) with a CanvasText outline, and status icons paint in CanvasText. The preview on sheet 2 is a simulation; real Windows contrast themes still need a check in block 5.
8. **Values on hover and focus:** the mockups use each mark's `<title>`. Go should also give marks `tabindex="0"` and an `aria-label` (done in `reference/memory.html`); the canvas cannot show focus states.
9. **Copy deck wording** stays within the plain labels; small changes: banner titles name the axis first ("Right: can't judge yet.", "Found needs attention."), the reason underneath carries the copy-deck sentence; attention items gained a "why this matters" text each; the updated page says how to restart ("Stop this dashboard (Ctrl+C) and start it again").
10. **Numbers the issue does not give were invented** to stay consistent with the scenarios: session lists and ids (`a41c09e2`…), memory ids (`7f3a9c21`…), project list (`backsight`, `project-a`, `project-b`, `project-c`, `project-d-shared-frontend-tools` to show truncation), bank size (1,284 memories, 286 documents), 30-day chart series, server costs over 30 days ($186.40), release rows, and the Day page. No memory text appears anywhere.
11. **Developer metrics on the Quality page** (used.share_confident, used.rank, used.by_path, search.rescue_rate, used.rubber_stamp, recall.by_lang) are not drawn there; developer mode is shown on the Overview (metric ids on tiles plus a "How well is memory ranked?" card), on Releases and on Definitions. (Note, 2026-11-10: as built, the "How well is memory ranked?" card and the per-path charts are on Quality, in developer mode.)

## Limits of this draft

- The canvas was not opened and checked visually after publishing: the Claude Design type forbids rendering it without asking first. The two static references were rendered once headless (Overview light at 1440 and 390 px, Memory dark at 1440 px) and the CSS was corrected from that look (tile alignment, legend icon size, Lately column width); the canvas uses the same stylesheet. Artboard heights are estimates: a page taller than its frame is clipped on the canvas but complete in full-window view.
- Mockup pages are built from masters with `<dc-import>`; to edit one tile for every scenario, edit `Tile`.

## Block 6 additions

Draft for issue #118 (scope items 1, 3, 4, 5; decision 0079 §4 rows marked block 6, §7 Preferences, §8, §9, §11 and the block 5 "as built" amendments), on the same canvas, in five new rows titled "Block 6: …", each with a sticky note listing the states it shows. The author approved it on 2026-11-02 (canvas version 43), with the rulings listed below; block 6 builds from it.

**Nothing drawn for block 3 changed.** Three masters gained props that are off by default, so every block 3 artboard renders exactly as before: `TopBar` (`b6`: custom window, two Preferences groups; `custom`, `density`, `keys`, and `open` values `custom`, `menucustom`), `Tile` (`ttr`: the Flowing tile's time-to-recall subline) and `LineChart` (`markers`). When block 6 is built, that behaviour becomes the default. `reference/dashboard.css` gained one appended section, "Block 6 additions (draft)", mirrored from the canvas (their first comment lines differ, as before); nothing above it changed. It opens with the calendar's four colour steps (`--hm-1..4`, light and dark): they belong in `tokens.css`, but `design/tokens.css` must stay byte-identical to the shipped `assets/tokens.css` (`TestTokensMatchTheDesign`), so they moved there on the day block 6 was built (2026-11-02; `design/tokens.css` and `assets/tokens.css` carry them, the reference keeps its section as drawn). `tokens.css` is unchanged.

| Row | Artboards (`B6*`) |
|---|---|
| Block 6: Overview additions | `B6Overview` (healthy, light) and `-dark`, `-down` (server down), `-390`, `-390-dark`: the Overview with Copy summary beside the summary line, the Flowing tile's new subline, Helped most next to "How big is the memory?", and the days calendar under them (§8 item 7). `B6OverviewParts`: Helped most normal, fewer than 3, none yet, server down; Copy summary with scripts on, after a click, with scripts off; the copied text. `B6Helped`: the card's master. |
| Block 6: Days calendar heatmap | `B6Heatmap` (master: 13 months, legend, today, window, a focused day, table), `B6Days` (Days list with the calendar, table open) and `-dark`, `-390`, `-390-dark`. |
| Block 6: custom window, density, shortcuts | `B6Controls` and `-dark`: custom window open, applied (range in the control, "window: Oct 1 to Oct 7" in the page head), invalid, inside the Menu at 390 px; Preferences with Density and Keyboard shortcuts (defaults, then changed); shortcuts off, on, and the list. `B6Overview-compact` (with Preferences open), `B6Sessions-compact`, `B6Overview-keys` (shortcuts on, `?` list open). |
| Block 6: Pipeline, System, Cost, Bank, Memory page, developer | `B6Funnel` (master), `B6Pipeline` and `-dark` (the funnel normal, watch, never extracted, too few; the Flowing tile's subline, its value only, in an ok and a watch week). `B6System` and `-dark` (health checks over time, the secrets card in four states, its attention item). `B6Widgets` and `-dark` (memory yield on Cost; the server model's tokens on Cost and Bank growth, both over 90 days with the same gap; the Memory page's source session in four states: this PC, another PC, not known, never shown yet). `B6Dev` (developer: share of memory that came up, concentration with the top 10, corrections after memory, learned-ranking changes, the language test). |
| Block 6: Releases comparison, markers | `B6Releases` (picker; a matched result that differs with "Settings differ" and "Observational, not an experiment"; strata too few by days and by turns; raw and matched of opposite sign; a comparison too few; no clear difference; a time chart with release and setting markers). `B6Day` (Day page with its markers card). |

New widget titles, in the catalog's style (proposed `Question` for each new metric):

| Metric | Label | Question |
|---|---|---|
| Helped most (no metric id: `stats memories --sort used --limit 3`) | Helped most | Which memories helped most? |
| days view, calendar | Days calendar | Which days did memory help? |
| `pipeline.time_to_recall` | From session to recalled memory | How long until a session's memories come back? |
| `memory.yield` | Memories that proved useful | Do new memories turn out useful? |
| `secrets.suspects` | Possible secrets in memory | Could memory hold a secret? |
| `doctor.history` | Health checks over time | Have the health checks passed lately? |
| `bank.active_share` (dev) | Share of the memory that came up | How much of the memory ever comes up? |
| `shows.concentration` (dev) | How concentrated shows are | Do a few memories take all the shows? |
| `injection.contradicted` (dev) | Corrections after memory was shown | Does the user correct Claude after memory is shown? |
| `boost.set_changed` (dev) | Recalls the learned ranking changed | How often does the learned ranking change what is shown? |
| Releases comparison | Comparing two versions | Do two versions differ? |
| Release and setting markers (Day page) | Release and setting markers | Did anything change that day? |

### Author's rulings (2026-11-01)

1. Helped most's "Show text" as a link to the memory's text page: accepted.
2. `memory.yield` on Cost: accepted.
3. Time to recall: both measures stay, but "What should I do?" keeps only ONE item for slow extraction, the existing `server.extraction_delay` one (30 min). The time-to-recall item is removed from every artboard; its watch shows only on its own card; the Flowing tile's subline keeps the value with no watch icon; the Flowing verdict is unchanged.
4. Days calendar default "prompts that got memory": accepted.
5. Copy summary's "Summary as text" fallback with scripts off: accepted.
6. The Menu window-link bug is fixed in the shipped stylesheet in a separate PR: the block 6 section no longer carries its rule (`.tools .navmenu .seg`).

### Where this draft departs from the issue, the decision or the brief, and why

1. **Helped most's "Show text" is a link to the memory's text page, not an inline `<details>` that fetches** (accepted by the author). `/memories/<id>/text` only answers a navigation (`Sec-Fetch-Dest: document`, block 4 R4), the CSP forbids framing it (`frame-ancestors 'none'`), and the API never returns text. The link keeps the shipped rule: text read live, never kept, one place that shows it.
2. **`memory.yield` is on Cost, not Bank** (accepted by the author): decision 0079 §8 lists it under Cost, and its catalog family is Cost; the brief had Bank.
3. **The window phrase reads "window: Oct 1 to Oct 7"**, as `windowPhrase` prints it since block 5, not "From Oct 1 to Oct 7".
4. **The custom window form sends `from` and `to`, both days included.** A plain form cannot send the exclusive `until` the URL uses, so the server turns them into `since` and `until` and redirects to the same page. The form's `action` is the current page's path (`/quality`, `/cost`, …, not `/`), and hidden inputs carry what the URL keeps (project filter, developer mode, a `has` filter), since a GET form replaces the whole query string. The date fields carry `min` (13 months back) and `max` (today), and point at the error with `aria-describedby`. An invalid range (end before start, more than 13 months back, a future day, a day missing) renders the same page with its previous window and the picker open with the error and the dates kept. No script.
5. **The days calendar is an HTML grid of day links, not an SVG.** The same markup is weeks-as-columns at 1440 px and, under 900 px, rows of weeks with seven touch-size days; a phone shows the last 13 weeks (`.hm-old` hides the rest) and the table and the Days list hold the others. Colours are classes (`q0`–`q4`, `none`, `today`, `inwin`), no inline style. At 1440 px a day is 14 px with 3 px between days, under the 24 px of WCAG 2.2 SC 2.5.8: it passes through the exception for an equivalent control on the same page, since the table under the calendar and the Days list hold a real link for every day. Under 900 px days take the 40 px touch size.
6. **The calendar is one Tab stop** (today's day; the others `tabindex="-1"`), arrow keys move day to day with scripts on, Enter opens the day: block 5 R5 makes a chart with more than 100 positions one Tab stop. The roving `tabindex` is rendered by the server, so with scripts off only today's day is reachable by Tab; that is acceptable only because the table and the Days list carry every value and link.
7. **The calendar colours by "prompts that got memory" by default** (accepted by the author), with "turns where Claude used memory" and "sessions" as links. Used turns are about 16 a week on the author's PC, so most days would be 0 or 1 and the calendar would show little.
8. **Time to recall's watch shows on its own card only** (author's ruling). The Flowing tile's subline carries the typical value with no watch word or icon; "What should I do?" gets no time-to-recall item, since slow extraction already has one, from `server.extraction_delay` (slowest 1 in 20 over 30 min); the Flowing verdict is unchanged. Both measures stay.
9. **The funnel's times are on a logarithmic scale** (1 min to 7 d): the three steps take minutes, minutes and a day. The whole-way time is computed on its own, not summed, and the card says so.
10. **The secrets card has two states the issue does not name**: "no scan yet" and "old scan" (the threshold, 30 days, is a proposal). Scope chip: server (the scan reads the server's memories).
11. **Doctor history marks one run of the daily upkeep per day** (`jobs maintain` appends to `doctor-history.jsonl`); a day without a run is a dashed ring, which is not a fault. (Note, 2026-11-10: the record is one file per month, `doctor-history-<YYYY-MM>.jsonl`.)
12. **The Releases comparison has a metric picker** (used, found, reported wrong; default "Turns where Claude used memory", since the 30-turn rule counts declared turns), and strata are "project × where memory was shown". Its readings, "differs" and "no clear difference", are worded as the issue asks and drawn as a neutral pill with the info icon, never a status colour. When raw and matched differences disagree, both are printed and the text says to read the matched one. The drawn case reconciles: all turns, unmatched, v0.1.16 187/326 = 57 % against v0.1.17 154/295 = 52 % (−5 pts); matched on the two strata both versions fill, backsight·prompt 70 % → 77 % and project-a·prompt 30 % → 46 %, weighted by ab/(a+b) (46 and 51), give 49 % → 60 % (+11 pts). v0.1.17 ran mostly on project-a (190 of 295 turns), where the share is lower, so the raw share falls while each stratum rises. The arithmetic is on the row's sticky note.
13. **Copy summary without scripts:** the button is hidden like every copy button, and the Overview gains a "Summary as text" disclosure holding the same plain text, so the summary can still be selected by hand (accepted by the author).
14. **Keyboard shortcuts' hint:** off by default, nothing on the page; on, a footer button "? Keyboard shortcuts" opens the same list. Keys are ignored with Ctrl, Alt or Meta and in any field (WCAG 2.1.4, character key shortcuts).
15. **Density is a set of spacing tokens** redefined on `.page.d-compact`; touch targets keep their size under 900 px. The cookie format changes (`v1.<theme>.<dev>.<refresh>` gains two fields, density and shortcuts). A four-field cookie written by block 5 must still parse: the two missing fields take their defaults (comfortable, shortcuts off), and the next save writes six.
16. **Bank growth and the server model's tokens past 30 days** (`bank.growth` and `extraction.tokens`, both read from `server-daily.jsonl`; note, 2026-11-10: one file per month as built, `server-daily-<YYYY-MM>.jsonl`) draw a "no record" band where `jobs maintain` could not reach the server, and a dotted line where the source changes from this PC's daily record to the server's last 30 days. The tokens chart is drawn on Cost (two server tasks stacked) with the same band, line and labels as Bank growth.
17. **A shipped bug found while drawing the Menu:** under 900 px the window links inside the Menu are hidden by `.tools .seg` (the Menu sits inside `.tools`), so a phone cannot change the window. The fix shipped separately, in the shipped stylesheet; the block 6 section carries no rule for it.
18. **Helped most follows the selected window** ("Used most in the last 7 days"), not the verdict's fixed 7 days.
19. **Touch targets under 900 px are 40 px** (`--touch`, decision 0079 §9 and the shipped stylesheet), not the 44 px the brief quotes: the calendar's days, the custom window's fields and Apply use the same token.
20. **No server-down state is drawn for the funnel, memory yield and secrets cards.** With the server down they keep their last numbers under the scope line's existing "not answering since …" state (block 5); nothing new is drawn. Helped most and the Overview show the state in `B6Overview-down` and `B6OverviewParts`.
21. **No dark artboards for `B6Releases`, `B6Dev`, `B6Day` and `B6Sessions-compact`.** They use only checked tokens and masters already drawn dark (`TopBar`, `LineChart`, tables, pills); block 3 also drew Releases and Day light only. `B6Dev` had a dark board in block 3; its new cards reuse the dark-checked marks of `B6Widgets-dark`.
22. **The Releases page keeps its shipped title, "Did a release or a setting help?"** (`views.Questions`). The review suggested "Did anything change between versions?" as less causal; changing a shipped question is outside this block's design, and the comparison's own wording ("differs", "no clear difference", "Observational, not an experiment") stays non-causal.
23. **A memory-yield week is complete only when its last day is more than 28 days before the window's end** (on Oct 30 the week of Sep 22 is the latest complete one; Sep 29 ends Oct 5, so it completes after Nov 2). Incomplete weeks are drawn as a band, never a bar.
24. **Info popovers outside developer mode name the metric id only** (`pipeline.time_to_recall`, `memory.yield`, `doctor.history`, `secrets.suspects`), as block 3 does: no p50/p95, cohorts or file names.

### Colour-blind check (block 6)

The only new colours are the calendar's four steps. They encode magnitude, not identity, so they were run through the validator's ordinal mode (one hue, lightness monotone, visible steps, light end clearing the surface). Its step check measures OKLCH lightness L and requires 0.06 between neighbours (it prints "all gaps >= 0.06"); in relative luminance the light ramp's last gap is smaller (0.057, #1F55A0 to #123566), which the check does not use, which passes in both themes; adjacent-pair CVD ΔE does not apply to a one-hue ramp. Every other new mark reuses a checked token.

| Ramp | Theme | Steps (`--hm-1..4`) | Validator (`--ordinal`) |
|---|---|---|---|
| days calendar | light (#FFFFFF) | #5C95E3 #2F72CC #1F55A0 #123566 | all pass: monotone, adjacent gaps in OKLCH L ≥ 0.06 (smallest 0.100), light end 3.06:1, hue spread 1° |
| same | dark (#161B22) | #3A6AA8 #4C88D8 #7DB1F1 #BBD9FB | all pass: monotone, adjacent gaps in OKLCH L ≥ 0.06 (smallest 0.103), light end 3.14:1, hue spread 5° |

- "Nothing recorded" and "0" differ by shape, not colour: a 1.5 px dashed edge versus a 1 px solid edge, both in `--c-shown` (3:1 or more in both themes).
- Health-check marks reuse the status colours, which are not separable under deuteranopia (watch vs attention, ΔE 3.1, block 3 table); each status has its own shape (circle, triangle, octagon; no run: dashed ring), the legend and the table carry the word.

### Contrast (block 6)

No new text colour: errors use `--bad`, key caps `--fg` on `--surface-2`, the primary button `--surface` on `--fg`, all already in the block 3 table. Graphics against the card surface (requirement 3:1):

| Mark | Light | Dark |
|---|---|---|
| calendar `--hm-1` | 3.06 | 3.14 |
| calendar `--hm-2` | 4.77 | 4.79 |
| calendar `--hm-3` | 7.33 | 7.77 |
| calendar `--hm-4` | 12.16 | 11.89 |
| calendar "0" edge and "nothing recorded" dashed edge (`--c-shown`) | 3.08 | 3.65 |
| health checks: "no run" dashed ring (`--c-shown`, 1.5 px) | 3.08 | 3.65 |
| today outline (`--fg`) | 15.80 | 14.64 |
| window ring (`--accent`) | 5.30 | 6.14 |
| health checks: ok, watch, attention fills | 5.92, 6.13, 6.09 | 7.55, 7.91, 6.86 |
| release and setting markers (`--fg-2`) | 7.82 | 9.50 |

Every mark that carries meaning clears 3:1 in both themes; each day also has its value in its label and title, the legend names every step, and the table lists every day. In forced colours the "no run" ring uses `CanvasText`, the "no record" band a `GrayText` dashed outline, and the chosen custom window a `Highlight` underline like the window links.

### Limits of this draft

- The canvas was not opened or rendered after publishing (the Claude Design type forbids checking without asking). The files were checked statically only: balanced tags, valid `data-props`, and the published copies byte-identical to the local ones. Heights are estimates; the sheets' open menus sit in boxes sized by guess, so a popover may be clipped on the canvas.
- Numbers are invented and consistent within each board, not across boards; the calendar and charts use seeded pseudo-random series.
- The `?` list is drawn over the page with `.keys-shade`, which stands for `dialog::backdrop`; the canvas cannot show a real modal, focus or keys.
- Copy summary's text is a proposal; its exact lines are for block 6 to settle with the CLI's text output.

## Block 8 additions

Draft for issue #120 (scope item 1; decision 0079 §13, its privacy line in §15, and §7 and §8 as built in block 6), on the same canvas, in three new rows titled "Block 8: …", each with a sticky note listing the states it shows. The author approved it on 2026-11-06 (canvas version 46) with the proposed answer to each of the questions below, as drawn; block 8 builds from it.

**Nothing drawn for blocks 3 and 6 changed.** Three masters gained props that are off by default: `TopBar` (`b8`: the machines switch in the scope line; `machines` `this`|`all`; `b8scope` `issues`|`clean`|`host`|`sharingoff`|`single`|`down`; `open` value `pcs`), `Tile` (`machines`: the tile added up over all PCs) and `Banner` (`b8`: "(last 7 days, this PC)" and one paragraph in its ⓘ). Checked statically: with the new props off, every value the three masters computed before is unchanged (TopBar on 5 prop sets, Tile on all 42 scenario × slot pairs, Banner on its 8 variants), and the two template holes the Tile gained default to the old text. The canvas's `dashboard.css` gained one appended section, "Block 8 additions (draft)"; its copy is the last section of `reference/dashboard.css`, byte for byte as appended on the canvas, and is shipped in `assets/dashboard.css` (`TestStylesheetIsTheDesigns` checks it, with its two block-numbered comments reworded as for block 6; the separate draft file is gone). No new colour and no new token: `tokens.css` is unchanged. One rendering change was made while building the pages: on a window of more than 200 positions the faint count bars of a line chart and the thin bars of a stacked chart are drawn as one path for each fill, not a rect each (the same shapes and fills, a third of the bytes, so the pages stay under the size cap); its two rules, `.chart path.seg.seg-fine` and `.chart path.seg.seg-fine.f-pending`, are in `reference/dashboard.css` before the simulation block, and `TestStylesheetIsTheDesigns` now refuses a shipped rule the reference lacks (the rules the first pages added without a place in the reference are pinned in `testdata/css-rules-beyond-the-design.txt`).

| Row | Artboards (`B8*`) |
|---|---|
| Block 8: scope line, machines switch, CLI | `B8Scope` and `-dark`, `-390`, `-390-dark`: the scope line with this PC (default) and all PCs; "Which PCs" open; all PCs with a project filter; this PC's sharing off; only this PC shares (no switch); a client in remote mode; the server host; the server not answering; the CLI disclosure with and without `--machines all`; the flag in the terminal and the API. |
| Block 8: Overview and Cost in all PCs | `B8Overview` (all PCs, healthy) and `-dark`, `-390`, `-390-dark`; `B8Overview-filter` (project filter and a latency watch: the Speed tile as a bucket range at watch, the attention item this PC's). `B8Cost` and `-dark` (the detail page: latency per day as bucket bars with this PC exact, a By PC table; widgets that stay this PC's; the server's cost unchanged). `B8Parts` and `-dark` (Speed ok and watch from buckets beside this PC's exact tile; Context as an average; reported wrong leaving too few once added up; the project-filter chip; a PC with no activity, no PC with activity, a PC that stopped sending, documents ignored; the summary line and "Summary as text"). |
| Block 8: System's Machines section and the Memory page | `B8System` and `-dark`, `-390`, `-390-dark` (Machines with several PCs; this PC only; this PC's sharing off as an attention-style item; the server not answering; the This PC card gains its label and whether it shares). `B8Memory` and `-dark` (this PC's, the other PCs' and all PCs' counts from `shared-combined`; hidden on another PC; not counted elsewhere; sharing off; server not answering). |

### What follows the switch

Other PCs send per day: the numerators and denominators of the six tile metrics, the same per `binary_version` for Releases, latency in fixed buckets, `binary_version` and the OS. Everything else stays this PC's in all PCs, with its "this PC" chip.

| Widget | In all PCs |
|---|---|
| Six tiles, summary line, Days calendar (prompts with memory, used turns), Quality and Cost time charts of those metrics, Releases per version | Added up, chip "all PCs (4)", CLI with `--machines all` |
| Speed (`recall.latency`) | A bucket range, "approximate"; the status is exact (below) |
| Context (`context.per_session`) | The average per session (total ÷ sessions); the status is judged on this PC's high value and says so |
| Verdict banner, "What should I do?", the review queue, the Days calendar's "sessions" colouring, histograms, outcome mixes, where context is added | This PC's, labelled. The verdict also weighs this PC's backlog, doctor and server answers, which other PCs do not send |
| Lately, sessions, projects, memories, Helped most, the turn rail | This PC's: other PCs send no session, project or memory id (§15) |
| Server widgets (memory size, the server model's cost, server tasks) | Unchanged: already every PC's; their commands never carry the flag |
| Memory page, "Has it helped on your other PCs?" | Always shown beside this PC's counts, whatever the switch |

### Proposed widget titles

| What | Label | Question |
|---|---|---|
| System view, the machines table | Machines | Which PCs share their activity here? |
| Memory page, counts from `shared-combined` | Counts from your other PCs | Has it helped on your other PCs? |
| The scope-line switch (a control, not a widget) | Whose activity | (none) |

### Questions for the author

Each is drawn with the proposed answer; the author approved every proposed answer on 2026-11-06.

1. **Where does the switch live?** Proposed and drawn: in the scope line, replacing "Activity on **this PC**" with "Activity on [this PC | all PCs on this server (4)]". The scope line already states whose activity a page shows, it stays visible under 900 px (the top bar's tools fold into the Menu there), and the top bar has no room left at 1440 px. Other option: a third control in the top bar's tools, and a group in the Menu.
2. **Is "all" ever the default?** Proposed: never. `this` is the default (issue item 3); the choice is a URL parameter kept by every link, like the project filter, not a preference, so a fresh link always opens on this PC.
3. **With no other PC sharing, is the switch shown?** Proposed and drawn: hidden; the scope line stays as shipped and System says "Only this PC shares". `?machines=all` still works and reads "(1)".
4. **When this PC's sharing is off, can it still read the others?** Proposed and drawn: no, "all PCs" is not offered, since `feedback.share_ledger = false` already means "neither sends nor reads" for the shared counts (`docs/configuration.md`). The switch's second half becomes a link to Machines on System, which says how to turn it on. Other option: read the others' figures anyway and add this PC's local ones.
5. **Does the verdict follow the switch?** Proposed and drawn: no; it stays this PC's and says "(last 7 days, this PC)", with "What should I do?" (things only this PC can fix). Other option: judge Found, Right and Cost on the merged figures and Flowing on this PC.
6. **Context in all PCs: average, or a mergeable high value?** Proposed and drawn: the average, from the numerator and denominator the issue lists, labelled, with the status from this PC's high value. Other option: add size buckets in characters to the document, as for latency, so the high value merges like Speed.
7. **Speed in all PCs: a range or an estimate?** Proposed and drawn: the bucket range ("1.0–2.0 s, approximate"), which claims nothing the buckets do not hold. Other option: interpolate inside the bucket ("≈ 1.7 s"), labelled approximate.
8. **A project filter with all PCs.** Proposed and drawn, as the issue says: this PC's figures filtered, the other PCs' whole, with "other PCs: not filtered" on the scope line and "other PCs not filtered" on every merged tile. Other option: with a project filter, all PCs falls back to this PC and says why.
9. **Freshness rules.** Proposed and drawn: other PCs' documents are fetched every 6 h with the shared counts (`jobs tick`) and kept on this PC, so all PCs works while the server is down ("other PCs as fetched at 07:40"); a PC is stale after 2 days without a send (they send daily) and dropped after 30, as for the shared counts.

### Where this draft departs from the issue, the decision or the shipped pages, and why

1. **The scope line gains a switch**, two links in the window control's style (`.seg`), not text. "Activity on this PC" was a statement; with several PCs it has to be a choice, and a link works without a script.
2. **N in "all PCs on this server (N)" counts every PC with a valid, current document**, this PC included, also one with no activity in the window or one that is stale; the tiles say how many had activity ("on the 2 PCs with activity this week").
3. **This PC's part comes from its own index, not from its published document**: it is live, and the same figures as with this PC selected. "Which PCs" says "its own figures, live".
4. **Tile links read "This PC's 167 prompts →"** in all PCs: other PCs' events never reach this PC, so the link can only lead to this PC's.
5. **Speed's status is exact although its value is a range**: watch over 2.0 s and attention over 2.5 s are bucket edges (2000 and 2500 ms), so the bucket holding the slowest 1 in 20 decides the word without doubt. This needs buckets counted upper-inclusive (a 2000 ms prompt is in "up to 2000"), which matches "over 2.0 s".
6. **The Cost page draws latency per day as bars from one bucket edge to the next**, with this PC's exact value as a ring, and a "By PC" disclosure (short id, system, prompts, slowest 1 in 20, typical). The ring is this PC's own index; the bars are the merge.
7. **This PC's sharing off is an `info` item**, on System's "What needs a look here?" and the Machines card only, never on the Overview's "What should I do?": decision 0079 §8 keeps info items off the Overview, and off is a choice, not a fault. No `backsight config set` command exists, so the item names the setting (`share_ledger = true` under `[feedback]`, `backsight paths` for the file) and gives `backsight jobs tick` to send at once. `.pill.st-info` gains the `--learn-bg` tint the other pills have.
8. **An ignored document shows only the short id from its name**, and only when the name holds a valid machine id; nothing from its content reaches a page or the JSON: "1 document ignored: not valid" or "over the 1 MiB cap".
9. **The Memory page's other-PC counts are used and ignored only, recent-weighted**: `shared-combined` holds credits, misses and hidden times, not shows, and only their sum over the other PCs (which PC is not kept). Shows, the timeline and ranks stay this PC's. The card is always shown, not tied to the switch, as issue item 5 says "next to this PC's"; its CLI carries `--machines all`.
10. **The host's scope line adds ", this PC" after its local address** (`127.0.0.1:18888, this PC`), only in block 8, so a host and a client read differently at a glance.
11. **The Machines card's scope chip is "server"**: the list is read from the server's control bank. Its CLI is `backsight stats system`, with no flag: the list is the same either way.
12. **No 390 px boards for Cost and the Memory page**: they use the shipped one-column collapse and tables in `.tbl-wrap`; Overview, the scope line and System are drawn at 390 px.
13. **The Days calendar is not redrawn**: the Overview board shows its card head with the "all PCs" chip and a sentence; its colouring by sessions stays this PC's.

### Colour-blind check (block 8)

No new colour. The latency bars use `--c-used` and this PC's value an outline ring in `--fg`: the two differ by shape (filled bar, hollow ring), not hue, and the watch and limit lines keep their block 3 dash patterns. Stale and ignored states carry a word and an icon (clock, info) besides colour. The validator was not run again: no colour pair is new.

### Contrast (block 8)

| Mark or text | On | Light | Dark |
|---|---|---|---|
| latency bucket bar (`--c-used`) | card | 4.42 | 4.75 |
| this PC's ring (`--fg`, 2 px) | card | 15.80 | 14.64 |
| "all PCs: sharing is off here" (`--fg-3`) | the switch's inset (`--surface-2`) | 5.12 | 5.40 |
| "sharing off" and "info" pills (`--learn` on `--learn-bg`) | tint | 5.08 | 6.93 |
| stale PC in "Which PCs" (`--watch`) | popover (`--surface`) | 6.13 | 7.91 |

All from the block 3 table; every mark that carries meaning clears 3:1 and every text 4.5:1 in both themes. In forced colours the bars keep their fill as data with a `CanvasText` outline, the ring is `CanvasText`, and the off link is `GrayText`.

### Limits of this draft

- The canvas was not opened or rendered after publishing (the Claude Design type forbids checking without asking). The files were checked statically only: balanced tags, valid `data-props` (enum defaults among their options), every `renderVals` run once with sample props, and the 24 published files byte-identical to the local ones. Heights are estimates; "Which PCs" open sits in a box sized by guess.
- Numbers are invented and consistent within each board, not across boards (the Overview says 1 document ignored, the states sheet 2).
- Machine ids, systems and dates are samples; no real machine id, hostname, project or session of another PC appears anywhere.
- The canvas's inline styles (two legend swatch colours, sheet spacing) are mockup layout only; block 8's pages need classes, as for blocks 5 and 6.
