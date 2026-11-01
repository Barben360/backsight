# Dashboard design (block 3 draft)

First draft of the `backsight dashboard` UI for issue #115 (decision 0079 §8–§10). Block 5 translates it into Go `html/template`. The author approved it on 2026-10-30, including departures 1 ("3,600 chars"), 2 (the 9,000-character context limit) and 4 (Found tile watch while the Found axis is ok when the server is down) listed below.

- **Claude Design canvas**: https://claude.ai/artifact/FbCVvwY5S8pvPPp8nppE8E (private until shared from its Share menu)
- `tokens.css`: colours for both themes (ink, status, the three category sets, a one-hue sequential ramp, chart chrome), type scale on the system font stack, spacing, radii, layout sizes.
- `reference/dashboard.css`: the single stylesheet built on `tokens.css`. The canvas uses this same file, so the mockups and the references cannot drift apart.
- `reference/overview.html`, `reference/memory.html`: static HTML, no script. Open them straight from disk; they load the logos from `docs/assets/`.

`canvas/` holds read-only, byte-for-byte copies of the 26 canvas artboards (`*.dc.html`) the pages are built from, taken on 2026-11-01, the day block 5 started, so the scenarios' wording and numbers can be read offline and reviewed in a diff. The canvas above stays the source: a change to the design is made there and these copies are refreshed from it, never edited here. They are mockups that need the canvas's runtime (`support.js`, `<dc-import>`, inline styles): nothing embeds, serves or links them.

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
11. **Developer metrics on the Quality page** (used.share_confident, used.rank, used.by_path, search.rescue_rate, used.rubber_stamp, recall.by_lang) are not drawn there; developer mode is shown on the Overview (metric ids on tiles plus a "How well is memory ranked?" card), on Releases and on Definitions.

## Limits of this draft

- The canvas was not opened and checked visually after publishing: the Claude Design type forbids rendering it without asking first. The two static references were rendered once headless (Overview light at 1440 and 390 px, Memory dark at 1440 px) and the CSS was corrected from that look (tile alignment, legend icon size, Lately column width); the canvas uses the same stylesheet. Artboard heights are estimates: a page taller than its frame is clipped on the canvas but complete in full-window view.
- Mockup pages are built from masters with `<dc-import>`; to edit one tile for every scenario, edit `Tile`.
