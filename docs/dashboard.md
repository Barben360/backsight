# The memory dashboard

backsight can tell you whether its memory is working and what it costs. Every answer comes from `backsight stats <view>`: plain text for you, `--json` for an agent or a script. The views read the local metrics index (built from backsight's own usage events, the feedback journal and the local status files) and, for the server's side, a few read-only calls to the server. They print counts, times, ids, project and session names; never the text of a memory, a prompt or an error (`stats memories <id> --text` is the one exception, and only when you ask for it).

Start with `backsight stats overview`: it answers "is memory working?" in four lines and lists what needs your attention, each with the command that helps.

## Opening the dashboard

`backsight dashboard` serves the same views in a browser, on this machine only (`127.0.0.1`). It prints a one-time link:

```
Dashboard: http://127.0.0.1:<port>/?code=<code>
```

- **Open the link within 2 minutes.** The code works once. It signs your browser in, then the address drops the code. Open the link from a terminal or paste it in the address bar; a link clicked on another web page is refused.
- **Another link.** `backsight dashboard link` prints a new one for the running dashboard. Use it when the first expired or was used, or to sign in from another browser.
- **`--open`** starts your default browser with the link. The link is then on that program's command line, visible to process listings for a moment; the code works once and expires in 2 minutes.
- **`--port <n>`** picks the port (1024 to 65535). Without it backsight takes a free one and prints it in the link.
- **`--json`** prints `{url, port, expires_at}` instead of the line above (`backsight dashboard link --json` too).
- **It runs in the foreground.** Ctrl+C stops it. It also stops on its own after 2 hours without a signed-in request. Your browser is signed in for as long as the dashboard is used: a session ends after 2 hours without use, and at most 8 browsers are signed in at once (the oldest is dropped).
- **One at a time.** While a dashboard runs, `backsight dashboard` exits with code 20 and names its address; `backsight dashboard link` gives you a link to it.
- **Fresh numbers.** A view's numbers are computed at most every 30 seconds, whatever the number of pages and browsers; the server's answers are kept up to 60 seconds and the doctor's checks up to 5 minutes. The page header has a Refresh link, and a preference reloads the page every minute.
- **Changes to the configuration** (the server's address, the key, the prices) are read when the dashboard starts: press Ctrl+C and start it again.

## From another PC

The dashboard never listens on the network. To see it from another PC, forward a port over ssh. The port must be the same on both ends, because the dashboard only answers to `127.0.0.1:<its port>` and `localhost:<its port>`.

1. On the host: `backsight dashboard --port 8765`.
2. On the other PC: `ssh -L 8765:127.0.0.1:8765 <user>@<host>`, and leave it open.
3. Open the link the host printed, in the browser of the other PC. A new one: `backsight dashboard link` on the host.

The cookie then lives in the browser of the other PC, and other users of that PC can reach the forwarded port. Forward only to a PC you trust.

## What the pages show

Each page asks one question, and its cards answer parts of it. Overview, Quality, Cost, Bank and Pipeline are in the top bar, with System; Projects, Sessions and Memories are under Browse. Releases and Definitions appear in developer mode (see Preferences). Days opens from links in other pages.

| Page | The question it asks | What it shows | `stats` view |
|---|---|---|---|
| Overview | Is my memory working? | The verdict on four axes (Found, Right, Cost, Flowing) for the last 7 days, a summary line, six tiles with their change from the previous period and a small trend, what to do about it, how big the memory is and what happened lately. | `backsight stats overview` |
| Quality | Is it right? | Whether Claude used the memories shown, whether it says which it used, memories reported wrong, why some prompts got nothing, Claude's own searches and what became of each memory shown. | `backsight stats quality` |
| Cost | Is it worth it? | The context backsight adds per session and where it adds it, the time memory adds to a prompt and to a search, and what the server's model spent. | `backsight stats cost` |
| Bank | Is the memory healthy? | The memory's size by kind, its growth, memories per project, summaries waiting, facts stored, memories retired, hidden or restored, and which memories need a look. | `backsight stats bank` |
| Pipeline | Is it flowing? | Sessions sent, what waits on this PC and on the server, the time to turn a session into memories, the background jobs and the steps that crashed. | `backsight stats pipeline` |
| System | Is the service up? | The memory server, this PC, what needs a look here and the doctor's checks. | `backsight stats system` |
| Releases | Did a release or a setting help? | The numbers per backsight version, the settings in effect and a command that replays a setting. Read differences as hints, not proof. | `backsight stats releases` |
| Definitions | What does each number mean? | Every number: its question, what it counts, when it is watched and how many events it needs, grouped by axis (Found, Right, Cost, Flowing, memory, review). | `backsight stats defs` |
| Projects | Which projects use memory? | One row per project; a project's name opens its own page. | `backsight stats projects` |
| Sessions | Which sessions used memory? | One row per session, which you can filter; an id opens that session turn by turn ("What happened in session ...?"). | `backsight stats sessions` |
| Memories | Which memories were shown? | One row per memory, with an order and a filter and badges for what to review; an id opens that memory ("How has memory ... done?"). | `backsight stats memories` |
| Days | How did each day go? | One row per day; a date opens that day ("What happened on Friday, Oct 30?"). | `backsight stats days` |

Each card is titled with the question it answers. Its info button says what the number counts, its limits and the smallest change it can show, and its CLI button gives the `backsight stats` command that prints the same numbers.

**Window and project.** Most pages have a window picker (24 h, 7 d, 30 d, 90 d; 7 d by default) and a project filter, where you type a project's name or choose one from the Projects page. The Overview's verdict always judges the last 7 days. The page of one day shows that day and has no picker; the page of one session shows the days the session spans, and the page of one memory reads "all time", neither with a picker.

**Language.** Numbers and dates follow your browser's language setting. The words stay in English.

Every number on a page is in `backsight stats <view> --json`: that is what scripts and agents read. The dashboard's own address `/api/v1/<view>` returns the same JSON, but only to a browser that is signed in, to feed the pages; it is not an interface for scripts.

A memory's text is not on the pages. Its own page has a "Show text" link; the text is read from the server when you click and is never stored.

## Privacy

The dashboard listens on `127.0.0.1` only. Its pages load nothing from other sites; the dashboard itself reads your memory server, as `backsight stats` does, and runs the doctor's checks. The footer's help link opens GitHub only when you click it.

The sign-in cookie is sent by the browser to every program that answers on the same host name (`127.0.0.1`, or `localhost` if you signed in through it), on any port. A program that gets it can read every page, and the text of any memory, until the session ends (2 hours without use, or when the dashboard stops). Local development servers are the typical case: do not open untrusted local services in the same browser while you are signed in. [privacy.md](privacy.md#the-dashboard) has the details.

## When a number is missing

A card never shows a bare dash: it says why there is no number. These are the sentences, and what to do.

| The page says | It means | What to do |
|---|---|---|
| "12 of 140: too few to judge. 140 prompts; 200 are needed." | The number is shown but there are too few events to judge it. | Nothing is wrong. Use memory a while and look again. |
| "Nothing recorded in the last 7 days." | No event of this kind in the window. | Pick a longer window: the card offers the longer ones up to 90 days. If a longer window is also empty, the feature has not been used. |
| "Memory feedback is off (`feedback.enabled`). Quality numbers need it. Found, Cost and Flowing still work." | backsight is not recording which memories Claude used. | To see quality numbers, set `enabled = true` under `[feedback]` in `config.toml` ([configuration](configuration.md)). |
| "This PC runs the server only: activity is recorded on the PCs that use it." | This PC hosts the memory server and has no Claude Code activity of its own. | Open the dashboard on a PC that uses the memory. |
| "Memory server not answering since 14:02." (or without the time) | The dashboard could not read the server. Numbers that come from the server wait; the others stay. | Run `backsight doctor`; it gives the fix. |
| "No memory server is set up on this PC." | There is no server to ask. | Run `backsight setup`, or `backsight connect` to join one. |
| "This memory server does not provide it." | The server is older than the number needs. | Update backsight with `backsight update apply`; if the server runs on another machine, update it there. |
| "No data." | The source holds nothing to count. | Not a fault; look again after some use. |

A whole page can also be replaced by a short message:

| The page says | What to do |
|---|---|
| "Preparing the dashboard." (building the metrics index from this PC's history; the page refreshes every 2 seconds) | Wait; the first build takes seconds to a minute or two. |
| "An update is being applied." ("Try again in a minute.") | Wait, then use Try again. |
| "backsight was updated." (stop this dashboard and start it again) | Press Ctrl+C in its terminal and run `backsight dashboard`. |
| "The page took too long." | Use Try again. |
| "This link has expired." (a one-time link, used or older than 2 minutes) | Run `backsight dashboard link` for a new one. |

A page that is not found, a malformed request and a page that fails keep the page's own question as their title, with a sentence that says what is wrong; for a failure, run `backsight doctor`.

## Preferences

The Preferences menu in the top bar keeps three choices in a cookie in your browser, for a year. The server writes the theme into the page it sends, so the page opens in the right colours with no flash.

- **Theme.** System follows your system's light or dark setting; Light and Dark force one.
- **Developer mode.** Off by default. On, the top bar gets a Developer menu with Releases and Definitions; each card and tile names the id of the numbers it shows; Quality gains a card on how well memory is ranked and per-path charts of what became of each memory shown; Cost gains the extraction per session; a memory's page shows its learned ranking as a number; and the CLI commands carry `--dev`.
- **Auto-refresh.** Off by default. On ("Every minute"), the page reloads itself every minute. The Refresh link in the top bar reloads it once. A page left open for more than 5 minutes shows "Updated over 5 min ago. Refresh."

The dashboard changes memory in no way: a preference only changes what you see.

## Keyboard and screen readers

- **Skip to content.** The first Tab stop on every page is "Skip to content", which jumps over the top bar.
- **Tab order.** On the Overview it runs: Skip to content, the logo, the pages (Overview, Quality, Cost, Bank, Pipeline, Browse, System), the window picker (24 h, 7 d, 30 d, 90 d), the project filter, Refresh, Preferences, then the verdict's info and CLI buttons, and the cards from top to bottom. Menus are native disclosure controls: Enter or Space opens one.
- **Charts have a table.** Every chart has a "Show as a table" control with the same numbers as text, which a screen reader can walk. It needs no script.
- **Copy buttons need scripts.** Commands the page suggests have a Copy button. Without scripts the button is absent and the command is plain text you can select. With scripts, if the browser does not let the page use the clipboard, the button selects the command and says "Selected: press Ctrl+C".
- **Works without scripts.** Everything else, including the menus, the preferences and the table views, works with scripts off.
- **Contrast and colours.** Text has at least 4.5:1 contrast and chart marks at least 3:1, in the light and the dark theme. Status is never only a colour: it is also a word or an icon. In a high-contrast mode (forced colours) the page and its charts use your system's colours.
- **Small screens.** Under 900 px wide, buttons and links are at least 40 px tall, and the window and project choices move into the Menu button.

## What each view answers

| Command | Question it answers |
|---|---|
| `backsight stats overview` | Is memory working, and what needs my attention? |
| `backsight stats quality` | Does memory show up, and is what it shows right? |
| `backsight stats cost` | What does memory cost in context, time and server tokens? |
| `backsight stats bank` | How big is the memory, and what should I review or forget? |
| `backsight stats pipeline` | Is what I write reaching the memory? |
| `backsight stats system` | Are the checks, the server and the background jobs healthy? |
| `backsight stats releases` | Did a new backsight version change anything? |
| `backsight stats projects [<name>]` | Which projects use memory, and how well? |
| `backsight stats sessions [<id>]` | Which sessions got memory, and what happened in one? |
| `backsight stats memories [<id>]` | Which memories are shown, used, reported or never used? |
| `backsight stats days [<date>]` | What happened each day? |
| `backsight stats defs [<id>]` | What does a number mean, and when is it judged? |

Flags go after the view word: `backsight stats overview --days 30`, `--project <name>`, `--since` and `--until`, `--tz`. [commands.md](commands.md) lists them all.

## Reading the numbers

- **n is the sample.** Every rate comes with its count (`num` out of `den` in the JSON): "62% of 140 prompts" means 87 of 140. A rate on a small n moves a lot with one event, so backsight also shows an interval (a range the true value very likely lies in, 95% of the time). A wide interval means "not sure yet".
- **Too few.** Under a metric's minimum sample (the *Minimum n* column below) the number is shown but not judged: `too_few`. It is not a problem; it means there is not enough data yet. Use the memory a while and look again.
- **No data.** `no_data` (or `no_events`) means the source holds nothing to count, for example a server that was not asked or a feature never used. It is not a failure either.
- **Statuses.** `ok`, `watch` (worth a look), `bad` (act now). A rate is judged on its interval, not its point: a rate is watched only when even the favourable end of its interval is past the line, so a lucky or unlucky small sample does not raise an alarm. `info` marks counts to review that are not alarms.
- **Borderline.** The point value is past a fixed threshold but the interval still reaches the good side: the status keeps the interval's reading, and the number is marked as close to the line. Only fixed thresholds can be borderline, not comparisons with the weeks before.
- **Compared with before.** Some metrics are also compared with their usual level: the median of the four previous weeks (7-day windows), needing at least two of them with enough data. Falling clearly below it is a `watch`, whatever the window you selected.
- **Unreliable.** Quality numbers depend on Claude telling backsight which memories it used. When fewer than 80% of the turns that showed memory say so (and at least 20 turns were counted), the quality numbers carry the tag `unreliable` and the Right axis cannot be judged.
- **Not declared.** A turn that showed memory but ended with no statement of what was used. Such turns are counted apart, never as "unused".
- **Recent-weighted counts.** The review-queue counts (often shown but never used, hidden, reported wrong) weigh recent shows and credits more than old ones, so an old burst does not keep a memory on the list. They are listings to review (`info`), not alarms.
- **"Last 7 days" whatever the window.** The verdict and the attention list always judge the last 7 days against the 7 before, for the selected project. The window you choose (`--days`, `--since`) changes the displayed counts, charts and tables, not the verdict. A few metrics are judged on "now" (hook failures over the last 24 hours, the current local backlog); the window only sets their displayed counts.
- **The 28-day fallback.** Right needs enough turns to judge. When the last 7 days have too few, its metrics are judged on 28 days and tagged `judged_28d`; the axis says so after its own reasons.
- **This PC or the server.** Metrics marked *this PC* count only this machine's usage. Metrics marked *server (all PCs)* come from the server and cover every PC using it, so they ignore the project filter (tagged `not_filtered`). A server that does not answer never fails a view: its sections read `ok: false` with the reason. On a PC with no memory server set up, the server's numbers read `no_data` with the reason `not_configured` ("no memory server set up"), not a failure.
- **Partial.** `partial` means the figures are incomplete: the index was still catching up, a day was cut by the window, or part of a server read failed (the failed part is counted beside the total).

### The verdict

The verdict has four axes, each in one of five states:

| Axis | Question |
|---|---|
| Found | Does memory show up when it should? |
| Right | Is what it shows used and correct? |
| Cost | Is the context and time it adds reasonable? |
| Flowing | Is what I write reaching the memory? |

| State | Meaning |
|---|---|
| `ok` | Judged, nothing to do. |
| `attention` | At least one metric is at `watch` or `bad` (two windows in a row for most; at once for recall latency and for error-class metrics). Its `reasons` say which. |
| `too_early` | Every judged metric of the axis has too few data. |
| `learning` | Found and Right only: fewer than 7 days since the first typed prompt (a new install), or fewer than 20 typed prompts in the last 7 days. |
| `cant_judge` | Right only: too few turns declare what was used, so the numbers cannot be trusted. |

Flowing is judged from day 1: a failed send is never hidden behind "too few".

A document's status is the last one logged for it. A subagent's report is logged `queued` when it enters the notes outbox, then `ok`, `retry` or `quarantined` by the flush that sends it. Earlier versions logged nothing after `queued`, so for that history the outbox decides: a note logged `queued` with nothing after it waits only while it was queued at or after the outbox's oldest note; one queued before has left the outbox and counts as sent (in Sessions sent, Sends by result and Waiting on this PC). With the outbox empty, no such note waits.

### Attention and doctor

The attention list holds each item's `text`, `why` and the `command` that helps, loudest first. It includes every warning of `backsight doctor` that affects how memory works: all of them but "an update is available". A doctor error makes Flowing need attention and comes first among Flowing's reasons; the doctor's warnings, which never change an axis, come after everything else. When the server does not answer, the items that follow from it fold into one root cause with the time it began.

### Two meanings of `ingested_at`

- In a view's `index.ingested_at`: when the index was last caught up with the files, as of the start of that catch-up (so a catch-up that found nothing new still moves it).
- In `backsight stats index --json`: the last time a file's lines were actually written into the index.

The first answers "how fresh is what I see"; the second "when did something last arrive". `index.lag_seconds` says how far behind the files the index is; `index.partial` is true when the catch-up stopped at its time budget.

## The metrics

Each row is one number the views can show. The table is generated from the program's own catalog (`backsight stats defs` prints the same, with the question each answers and what to do when it is bad).

<!-- catalog:start -->
| Id | Label | What it counts | Unit | Status rule | Minimum n | Scope | Developer |
|---|---|---|---|---|---|---|---|
| `recall.hit_rate` | Prompts that got memory | Prompt recalls that put at least one memory in front of Claude, out of all prompt recalls. Shown with a 95% range; it is watched when even the interval's upper bound stays under 10%. | share | watch when even the upper end of the interval is under 10%; watch when clearly below 70% of its usual level (the median of the four previous weeks, once two of them have enough data) | 20 | this PC |  |
| `recall.empty_reasons` | Why prompts got nothing | Recalls that injected nothing, counted by reason (no candidates, below the cut-off, all already seen, capped), and prompts skipped, counted by reason. | count | shown, not judged | 0 | this PC |  |
| `recall.by_lang` | Prompts that got memory, by language | The share of prompt recalls that injected a memory, split by the language of the prompt. Each language needs its own 20 prompts before it is judged. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `search.rate` | Prompts where Claude searched memory | Typed prompts of at least 40 characters whose turn used memory_search, memory_read or memory_store (main agent), out of all typed prompts of at least 40 characters. | share | watch when clearly below 70% of its usual level (the median of the four previous weeks, once two of them have enough data) | 20 | this PC |  |
| `search.empty_rate` | Searches that found nothing | memory_search calls that succeeded with no hit, out of memory_search calls that succeeded. Failed calls are counted apart. | share | watch when clearly above its usual level (the median of the four previous weeks, once two of them have enough data) | 20 | this PC |  |
| `used.turn_rate` | Turns where Claude used memory | Main-agent turns that showed memory and whose final stop declared at least one memory used, out of those turns that declared (a turn declaring only wrong, outdated or none counts as declared without use). The worst case, counting undeclared turns as unused, is given beside it. Unreliable while the declaring coverage's upper bound is under 80%. | share | watch when clearly below 70% of its usual level (the median of the four previous weeks, once two of them have enough data) | 20 | this PC |  |
| `declare.coverage` | Turns where Claude said what it used | Main-agent turns that showed memory and ended with a declaration, out of those that ended declared or undeclared. Turns whose check was skipped are counted apart, and turns still open are pending. | share | watch when even the upper end of the interval is under 80% | 20 | this PC |  |
| `used.rubber_stamp` | Turns where every memory was marked used | Declared turns where every shown memory was credited, out of declared turns that showed at least two memories. Watched when even the interval's lower bound is above 80%. | share | watch when the lower end of the interval is over 80% | 30 | this PC | yes (`--dev`) |
| `used.share_confident` | Confidently ranked memories that were used | Settled shows credited as used, out of shows credited or confidently missed, for the prompt path by default. Shows with an unknown outcome, shows of memories forgotten since, and shows still pending are reported beside it. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `used.rank` | Position of the memories used | How the credited shows were ranked (1 to 5, 6 and beyond, unranked), and how often a used memory was the first, or among the first three, in turns with a credit. | count | shown, not judged | 20 | this PC | yes (`--dev`) |
| `used.by_path` | Used, by where memory was shown | The share of confidently ranked memories that were used, for each way memory is shown (prompt, search and others). Each path needs its own 20 shows. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `search.rescue_rate` | Memories Claude found itself | Credits on the search path in turns where the prompt recall also showed memories, out of all credits in those turns. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `verdict.wrong_rate` | Memories reported wrong or out of date | Wrong and out-of-date verdicts, out of used, wrong and out-of-date verdicts. Watched when even the interval's lower bound is above 5%, bad above 15%. | share | watch when the lower end of the interval is over 5%, bad over 15% | 20 | this PC |  |
| `stale.never_used` | Often shown, never used | The memories shown often without ever being used, counted from the feedback ledger with recent feedback weighted more. | count | shown, not judged | 0 | this PC |  |
| `stale.hidden` | Hidden | The memories hidden from recall, counted from the feedback ledger with recent feedback weighted more. | count | shown, not judged | 0 | this PC |  |
| `stale.reported` | Reported wrong | The memories Claude reported wrong or out of date, counted from the feedback ledger with recent feedback weighted more. | count | shown, not judged | 0 | this PC |  |
| `context.per_session` | Context added per session | Characters backsight added to Claude's context per session (prompt recall plus briefing), typical (median) and high (slowest 1 in 20), with approximate tokens (characters divided by 4). Watched when the high value passes 1.5 times the session budget. | chars | watch when the slowest 1 in 20 is over 1.5 times the configured session budget | 100 | this PC |  |
| `context.per_prompt` | Context added per prompt that got memory | Characters injected by prompt recalls that put memory in front of Claude. The value is the typical (median) size; the high value (slowest 1 in 20) is given beside it once there are 100 recalls. | chars | shown, not judged | 20 | this PC |  |
| `recall.latency` | Time memory adds to a prompt | Wall time of the prompt recall, typical (median) and slowest 1 in 20. Watched above 2,000 ms, bad above 2,500 ms. | milliseconds | watch when the slowest 1 in 20 is over 2000 ms, bad over 2500 ms | 100 | this PC |  |
| `search.latency` | Time of a memory search | Time of a memory_search call, typical (median) and slowest 1 in 20. Watched above 3,000 ms. | milliseconds | watch when the slowest 1 in 20 is over 3000 ms | 100 | this PC |  |
| `extraction.tokens` | What the server's model spent | Tokens (input, of which cached, and output; the total is input plus output plus thoughts, which count with the output, so it is a little above the server's own total) the server's model spent per day and per operation, for all PCs using the server. The server keeps 30 days. | tokens | shown, not judged | 0 | server (all PCs) |  |
| `extraction.cost` | What the server's model spent | The tokens spent, priced from the [llm] prices in your backsight config; it reads "price not set" when no price is set. For all PCs using the server. | money | shown, not judged | 0 | server (all PCs) |  |
| `extraction.per_session` | Extraction per session sent | Extraction tokens of the last 28 days divided by the sessions sent successfully in those days, as of now whatever the window. The tokens are every PC's on the server; the sessions are this PC's, so on a shared server the figure is too high. | tokens | shown, not judged | 0 | server (all PCs) | yes (`--dev`) |
| `bank.size` | Memories | Memories by kind (facts about you and your work, facts from sessions, summaries) and the documents they come from. | count | shown, not judged | 0 | server (all PCs) |  |
| `bank.growth` | New memories per day | Memories added per day, by kind, from the server's time series. | count | shown, not judged | 0 | server (all PCs) |  |
| `bank.by_project` | Memories per project | Memories per project, from the server's project tags. | count | shown, not judged | 0 | server (all PCs) |  |
| `bank.consolidation` | Summaries waiting | Summaries pending and failed, and when the last one was built. The failed count is the server's current total. Watched when any failed. | count | see What it counts | 0 | server (all PCs) |  |
| `bank.churn` | Retired, hidden, restored | Forget, retire, hide and restore actions recorded per week. | count | shown, not judged | 0 | this PC |  |
| `notes.stored` | Facts stored | Store events by result (saved, updated, refused) and by kind. | count | shown, not judged | 0 | this PC |  |
| `pipeline.flow` | Sessions sent | Documents whose last status is ok, out of documents with a send in the window. The interval counts documents (a session's documents per project), not independent sends. Failed, set aside and waiting ones are given beside it. A note logged queued with nothing after it waits only while the notes outbox still holds it (queued at or after its oldest note); one that left the outbox counts as sent. Watched when any failed or was set aside; bad when the backlog is older than 24 hours. | share | see What it counts | 0 | this PC |  |
| `retain.outcomes` | Sends by result | Per day, documents by their last status, a queued note that left the notes outbox counted as sent and a send logged without a status as unknown. The duration (slowest 1 in 20) counts successful sends only. | count | shown, not judged | 0 | this PC |  |
| `local.backlog` | Waiting on this PC | Notes queued, sessions waiting to be sent, items set aside, and the age of the oldest (a note logged queued counts only while the notes outbox holds it). Watched when the oldest is over 1 hour, bad over 24 hours. | count | see What it counts | 0 | this PC |  |
| `server.queue` | Waiting on the server | Operations queued and running on the server, from its operations list. The failed count covers operations updated in the last 7 days, whatever the window; when the server holds more failed operations than it lists, the count is a lower bound. Watched when any failed. | count | see What it counts | 0 | server (all PCs) |  |
| `server.extraction_delay` | Time to turn a session into memories | Time between a completed operation's creation and its last update, typical (median) and slowest 1 in 20, over the last 200 in the window. Watched above 30 minutes. | milliseconds | watch when the slowest 1 in 20 is over 30 min | 100 | server (all PCs) |  |
| `hooks.failures` | Steps that crashed | Hook failures by kind (panic, fatal, timeout, killed, exit, invalid output, input too large, spawn). In the last 24 hours, a panic, fatal, exit or spawn failure, or any kind three times, is watched; ten or more is bad. | count | see What it counts | 0 | this PC |  |
| `server.reach` | Memory server answering | The last answer, since when it is failing, and until when recall stays paused after repeated failures, as this PC recorded them, with the prompts skipped while it was paused. Watched when failing or while recall is paused; bad when a backlog older than 24 hours builds up with it. | count | see What it counts | 0 | this PC |  |
| `doctor.check` | Health checks | The doctor's checks, one entry per check with its status. Watched when any check is an error. | count | see What it counts | 0 | this PC |  |
| `jobs.last_run` | Background jobs | The last tick and maintain runs, with status and duration. Bad when a job is more than twice its cadence late and prompts or tool events happened since it was due (a sleeping laptop is not a fault). | milliseconds | see What it counts | 0 | this PC |  |
<!-- catalog:end -->

## For agents

An agent answers "is memory working?" with `backsight stats overview --json`, then drills down by view. The [agent guide](agent-guide.md#is-memory-working) says which fields to read.
