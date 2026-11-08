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
| Overview | Is my memory working? | The verdict on four axes (Found, Right, Cost, Flowing) for the last 7 days, a summary line with a Copy summary button, six tiles with their change from the previous period and a small trend, what to do about it, what happened lately, how big the memory is, the memories that helped most and a calendar of the days memory helped. | `backsight stats overview` |
| Quality | Is it right? | How many prompts got memory, whether Claude used the memories shown, whether it says which it used, memories reported wrong, why some prompts got nothing, Claude's own searches and what became of each memory shown. | `backsight stats quality` |
| Cost | Is it worth it? | The context backsight adds per session and where it adds it, the time memory adds to a prompt and to a search, and what the server's model spent. | `backsight stats cost` |
| Bank | Is the memory healthy? | The memory's size by kind, its growth, memories per project, summaries waiting, facts stored, memories retired, hidden or restored, files imported from a folder, and which memories need a look. | `backsight stats bank` |
| Pipeline | Is it flowing? | Sessions sent, what waits on this PC and on the server, the time to turn a session into memories, how long until a session's memories come back, the sessions sent that never turned into memories, the background jobs and the steps that crashed. | `backsight stats pipeline` |
| System | Is the service up? | The memory server, this PC, what needs a look here, the doctor's checks, how they did day by day and whether the last scan found a possible secret. | `backsight stats system` |
| Releases | Did a release or a setting help? | The numbers per backsight version, a comparison of two versions, the days Claude used memory, the settings in effect and a command that replays a setting. Read differences as hints, not proof. | `backsight stats releases` |
| Definitions | What does each number mean? | Every number: its question, what it counts, when it is watched and how many events it needs, grouped by axis (Found, Right, Cost, Flowing, memory, review). | `backsight stats defs` |
| Projects | Which projects use memory? | One row per project; a project's name opens its own page. | `backsight stats projects` |
| Sessions | Which sessions used memory? | One row per session, which you can filter; an id opens that session turn by turn ("What happened in session ...?"). | `backsight stats sessions` |
| Memories | Which memories were shown? | One row per memory, with an order and a filter and badges for what to review; an id opens that memory ("How has memory ... done?"). | `backsight stats memories` |
| Days | How did each day go? | A calendar of the last 13 months, then one row per day; a date opens that day ("What happened on Friday, Oct 30?"). | `backsight stats days` |

With other PCs sharing, a switch in the scope line adds them up: see [Several PCs](#several-pcs).

Each card is titled with the question it answers. Its info button says what the number counts, its limits and the smallest change it can show, and its CLI button gives the `backsight stats` command that prints the same numbers.

**Window and project.** Most pages have a window picker (24 h, 7 d, 30 d, 90 d; 7 d by default, or a custom window, see below) and a project filter, where you type a project's name or choose one from the Projects page. The Overview's verdict always judges the last 7 days. The page of one day shows that day and has no picker; the page of one session shows the days the session spans, and the page of one memory reads "all time", neither with a picker.

**Custom window.** The picker's last entry, Custom window, takes a From and a To day. Both days are included, and the first can be up to 13 months back, as far as this PC keeps its history. A pair of days that does not fit (a To before the From, a day in the future or too far back) is refused with the reason, and the picker stays open with what you typed. The window's days then appear in the page's address as `since` and `until`, like the flags of `backsight stats`.

**Language.** Numbers and dates follow your browser's language setting. The words stay in English.

Every number on a page is in `backsight stats <view> --json`: that is what scripts and agents read. The dashboard's own address `/api/v1/<view>` returns the same JSON, but only to a browser that is signed in, to feed the pages; it is not an interface for scripts.

A memory's text is not on the pages. Its own page has a "Show text" link; the text is read from the server when you click and is never stored.

## What the newer cards show

These cards come from the daily upkeep's record (see the notes of v0.1.28-m7 in [releases](releases/v0.1.28-m7.md)) and from the numbers added to `backsight stats` then. A card that has too little history says so in its own words rather than showing a number.

**Overview**
- **Which memories helped most?** The 3 memories Claude said it used most in the window, each with its project, its kind and how often it was used and shown. A name opens the memory's page.
- **Which days did memory help?** A calendar of the last 13 months, a column per week. Colour by picks what the squares show: Prompts that got memory, Turns where Claude used memory or Sessions. A dashed square is a day with nothing recorded; the window's days and today are outlined. Show as a table lists the same days as text, and a day opens its page. It does not follow the window: it always covers 13 months, for the project you filtered by.
- **Copy summary** copies the page's numbers as plain text, 72 characters wide at most, in English whatever your browser's language, ready to paste into an issue or a chat. The same text is in the Summary as text disclosure at the foot of the page, which works without scripts. It holds counts, rates, statuses and commands, the project name when you filtered by one, the window's dates, and the backsight version and the time it was written: no memory text, no session or memory id. The memory server's address shows only when it is on this PC (any loopback address, `::1` included); any other is written "a remote server".
- **Flowing tile.** Its subline gains the typical time from a session's last turn to its first memory recalled, when there are enough sessions to say.

**Days and one day**
- The Days page opens with the calendar above, filtered by the project and not by the window.
- A day's page has links to the previous and the next day with something recorded, and **Did anything change that day?** lists each new backsight version and each settings change recorded that day, "recorded at" the hour the change was noticed.

**Pipeline**
- **How long until a session's memories come back?** For each session ended on this PC: how many were sent, turned into memories and recalled, and the time each step took (typical and slow). A session counts as ended 30 minutes after its last turn. A step shows its times only once it has 20 sessions (the slow time, 100), so a new install reads "Too few to time yet" with the counts.
- **Sent, never turned into memories** lists the newest of those sessions with what happened: the server's task failed, it finished with no memory, or nothing was recorded. Sessions still being turned into memories (sent less than 24 hours ago) are not in it.

**System**
- **Have the health checks passed lately?** Each check's result for each day, from the daily upkeep's run: ok, watch or problem. A day without a run is shown as none, which is not a fault. Checks that had a non-ok day come first.
- **Could memory hold a secret?** Four states: no scan yet, a scan that found nothing, a scan that found some (with the command to list and forget them) and a scan older than 30 days, which is only a reminder. A scan that found some puts the Overview's Flowing axis on watch.

**Cost and Bank**
- **Do new memories turn out useful?** (Cost) Per week, the share of the memories created that week that Claude used within 28 days, and the extraction tokens per useful memory. A week counts once its 28 days have passed, so the newest weeks wait. The memories come from the server and the uses from this PC: with several PCs the share reads low.
- Charts of days the server no longer keeps (Bank: 90 days; Cost: 30) fill from this PC's daily record, and say where. Days from neither the server nor the record are shaded, and "Show as a table" says for each day where it comes from.

**Memory page**
- **Where did it come from?** The session the memory was made from, when it was sent and turned into memories, and when it was first shown. A memory from a session of another PC says "made" and its day; a note or an import has no session.

**Developer mode** adds these cards: the share of the memory that came up and how concentrated the shows are (Bank); the share of turns followed by a correction and the share of recalls the learned ranking changed (Quality); and, in **Does memory show up as often in every language?**, whether a language differs from the most used one by more than chance (a gap of more than 15 points is watched). The learned-ranking share undercounts recalls from before it was recorded, until `backsight stats index rebuild`.

**Releases** (developer mode)
- **Do two versions differ?** Pick two versions and a measure. The answer is a difference with its 95 % interval, matched by project and by where memory was shown, read as differs, no clear difference or too few, with what is missing and when to look again. It is observational: the two versions ran on different weeks, work and settings, so it never says one is better. When the raw numbers point the other way it says so.
- **When did Claude use memory?** The share of turns where Claude used memory over time, with a marker at each version switch and settings change recorded on this PC.

## Several PCs

When other PCs use the same memory server and share their activity (`feedback` and `feedback.share_ledger`, both on by default; [what they send](privacy.md#what-your-pcs-share-with-each-other)), the pages can add them up. The same figures are in `backsight stats overview --machines all` and the other views ([several-pcs.md](several-pcs.md#see-all-your-pcs-activity)).

**The switch.** The line under the top bar reads `Activity on` and a choice: **this PC | all PCs on this server (N)**, where N counts this PC and every PC whose figures were read. It appears when another PC shares; with none, the line is the one a single PC always had. The dashboard reads the sharing settings (`feedback`, `feedback.share_ledger`) when it starts, like the rest of the configuration: after turning sharing on or off, stop the dashboard (Ctrl+C) and start it again, or the switch keeps the answer it had. Choosing all PCs puts `machines=all` in the address: every link and form on the page keeps it, the **this PC** half removes it, and it is never a preference, so a bookmarked all-PCs address opens on all PCs. Under 900 px the switch stays in this line, on a line of its own, and does not move into the Menu.

- **Which PCs** opens beside the switch once all PCs is chosen. It lists each PC counted by the start of its machine id, with its system, the day it first sent and when it sent (`sent 5 h ago`, then `sent 9 d ago` from 48 hours on: the same words and the same rounding as the Machines card on System); a PC that sent nothing for 2 days is amber and says where its figures stop (`its figures stop on Oct 20`). When you open the dashboard on the PC that holds the memory server (it reaches the server on a loopback address), that PC's own row says `this PC, holds the memory server`; seen from another PC, the host's row is not marked. Below it: how many documents were set aside, when the figures were fetched, and a link to Machines on System.
- **Notes after the line** appear only when they apply: `other PCs: not filtered` with a project filter (the other PCs send no project, so their whole figures are added); `1 PC: nothing sent for 9 days`, with the PC and the day its figures stop in the tooltip; `1 document ignored`; and `other PCs as fetched at 07:40` while the memory server does not answer, since the list is then the one fetched earlier.
- **Sharing off** on this PC: the second half reads `all PCs: sharing is off here` and links to Machines on System, which says which setting is off. A PC that does not share does not read the others' either.
- **Projects, Sessions, Memories** and the page of one of them are always this PC's: the second half is plain text with the reason in its tooltip (`Projects, sessions and memories are this PC’s: other PCs send none`), and the figures stay this PC's. A page of this PC alone that you reach from a kept link has no `--machines all` in its commands.

**What follows the switch.** With all PCs, a card or tile that adds the PCs up wears the chip `all PCs (N)` in place of `this PC`; its command (CLI button) ends with `--machines all`. Everything else keeps the this-PC chip and its command without the flag.

| Where | Adds up all PCs | Stays this PC's or the server's |
|---|---|---|
| Overview | The six tiles: Found, Used, Reported wrong, Context, Speed and Flowing; the calendar of the days memory helped (colour by sessions is this PC's: the others send no sessions) | The verdict, the summary's verdict lines, **What should I do?**, **What happened lately?**, **Which memories helped most?**, **How big is the memory?** (the server's) |
| Quality | The Found and Used cards | The outcomes, reports, empty reasons, searches and every list |
| Cost | The time memory adds to a prompt, drawn per day as a range with a **By PC** table; in the figures above, the context per session (an average) and that time | The context card's chart (the other PCs send a total and a count, not sizes), the time of a search, and what the server's model spent (every PC's) |
| Pipeline | The write flow (how many sends worked) | What waits on this PC and on the server, the jobs, the steps that crashed |
| Days, one day, Releases | The calendar and the day's tiles; the numbers per version | The day-by-day table, the markers on a day, the comparison of two versions |
| Bank, System | Nothing | Both are the server's or this PC's. System gains **Which PCs share their activity here?** (below) |

The tiles show **a range** where the PCs' times are only known by bucket:

- **Speed** reads as a range, for example `1.0–2.0 s`, marked `approximate`: the PCs send how many prompts fell in each time bucket, not each time, so the slowest 1 in 20 can only be placed between two bucket edges. This PC's exact value sits beside it. A change from the week before is said in buckets (`same bucket as the week before`, `one bucket slower`), and the tile says `Over 2.0 s for sure` or `Over 2.5 s for sure` when the whole range is past the limit. On Cost each day is a bar from the bucket's lower to its upper edge, with a ring for this PC's exact value, and the By PC table lists the range of each PC.
- **Context** is an average per session over the sessions of every PC (`average per session, all PCs`), with this PC's high value beside it; the `≈ N tokens` follows the average. It has no change from the week before.
- **Reported wrong** can say `too few` with this PC alone and enough once the PCs are added up.
- **Counts in links** stay this PC's: `This PC’s 167 prompts →` opens this PC's list, and Found's subline gives `this PC 46 of 167`.

**The verdict stays this PC's.** On all PCs the banner reads `(last 7 days, this PC)` and says why: it also weighs what only this PC knows, such as what waits to be sent, its health checks and the server's answers. **Copy summary** and **Summary as text** hold the added-up tiles, say `Activity on all PCs on this server (N)`, and name the notes above in one line.

**With no activity.** When none of the PCs had any in the window, the Overview says `No activity on any of the N PCs in the last 7 days` with the day of the latest one and links to a longer window.

**Machines.** System has a card, **Which PCs share their activity here?**, with one row per PC: its machine id's start, system, the day it first sent, when it last sent (`5 h ago`, `9 d ago`: whole days from 48 hours, as in Which PCs), its backsight version and whether it is counted in all PCs (`counted`, `up to Oct 20` for a PC gone quiet, with its days after that missing). A document set aside is a row of its own with the reason, `over the 1 MiB cap` or `not valid` (or `written by a newer backsight: update this PC`), and nothing else of it. Under the table: when the figures were fetched (every 6 hours), and that a PC that has not sent for 30 days drops off. With sharing off it names the setting to change and the command; when the memory server does not answer it says so and shows the list fetched earlier.

**A memory's page on other PCs.** **Has it helped on your other PCs?** shows how often Claude used the memory, or was shown it and ignored it, on this PC, on the other PCs and together, and whether it is hidden from recall on another PC (with the command that brings it back). It is the counts your PCs share through the memory server, which the learned ranking uses; it needs no switch.

## Keyboard shortcuts

Off by default; turn them on in Preferences. Then `g` followed by a letter within a second opens a page, and a footer button, or `?`, lists them all.

| Keys | Action |
|---|---|
| `g` `o` / `g` `q` / `g` `c` / `g` `b` / `g` `p` / `g` `s` | Overview / Quality / Cost / Bank / Pipeline / System |
| `/` | Filter by project |
| `r` | Refresh this page |
| `?` | This list |
| `Esc` | Close this list or an open menu |

Keys do nothing while you type in a field or with Ctrl, Alt or Meta held. On the calendar, the up and down arrows move one day, the left and right arrows one week, Home and End go to the first and last day, and Enter opens the day; these calendar keys work even with shortcuts off. All of them need scripts; everything they do is also a link or a button.

## Privacy

The dashboard listens on `127.0.0.1` only. Its pages load nothing from other sites; the dashboard itself reads your memory server, as `backsight stats` does, and runs the doctor's checks. The footer's help link opens GitHub only when you click it.

The sign-in cookie is sent by the browser to every program that answers on the same host name (`127.0.0.1`, or `localhost` if you signed in through it), on any port. A program that gets it can read every page, and the text of any memory, until the session ends (2 hours without use, or when the dashboard stops). Local development servers are the typical case: do not open untrusted local services in the same browser while you are signed in. [privacy.md](privacy.md#the-dashboard) has the details.

## When a number is missing

A card with no number usually says why; a dash stands where a value simply does not exist yet. These are the main sentences that call for an action.

| The page says | It means | What to do |
|---|---|---|
| "12 of 140: too few to judge. 140 prompts; 200 are needed." | The number is shown but there are too few events to judge it. | Nothing is wrong. Use memory a while and look again. |
| "Nothing recorded in the last 7 days." | No event of this kind in the window. | Pick a longer window: the card offers the longer ones up to 90 days. |
| "Memory feedback is off (`feedback.enabled`). Quality numbers need it. Found, Cost and Flowing still work." | backsight is not recording which memories Claude used. | To see quality numbers, set `enabled = true` under `[feedback]` in `config.toml` ([configuration](configuration.md)). |
| "This PC runs the server only: activity is recorded on the PCs that use it." | This PC hosts the memory server and has no Claude Code activity of its own. | Open the dashboard on a PC that uses the memory. |
| "Memory server not answering since 14:02." (or without the time) | The dashboard could not read the server. Numbers that come from the server wait; the others stay. | Run `backsight doctor`; it gives the fix. |
| "No memory server is set up on this PC." | There is no server to ask. | Run `backsight setup`, or `backsight connect` to join one. |
| "This memory server does not provide it." | The server is older than the number needs. | Update backsight with `backsight update apply`; if the server runs on another machine, update it there. |
| "No data." | The source holds nothing to count. | Not a fault; look again after some use. |
| "Judged on the last 28 days: too few turns in 7." | The last 7 days hold too few turns to judge the Right axis, so it is judged on 28 days. | Nothing to do; the verdict is still valid. |
| A "too few" chip on a tile or axis | Fewer events than the number needs to be judged. | Same as the first row. |

Other lines say that a list or a card is empty, for example "No session in this window." (with "matches this filter" when a filter is on), "Nothing was reported wrong or out of date in the last 7 days.", "No step failed in the last 7 days." and "Nothing needs a look." They are good news or an empty window, not faults.

A whole page can also be replaced by a short message:

| The page says | What to do |
|---|---|
| "Preparing the dashboard." (building the metrics index from this PC's history; the page refreshes every 2 seconds) | Wait; the page refreshes itself. |
| "An update is being applied." ("Try again in a minute.") | Wait, then use Try again. |
| "backsight was updated." (stop this dashboard and start it again) | Press Ctrl+C in its terminal and run `backsight dashboard`. |
| "The page took too long." | Use Try again. |
| "This link has expired." (a one-time link, used or older than 2 minutes) | Run `backsight dashboard link` for a new one. |

A page that is not found, a malformed request and a page that fails keep the page's own question as their title, with a sentence that says what is wrong; for a failure, run `backsight doctor`.

## Preferences

The Preferences menu in the top bar keeps five choices in a cookie in your browser, for a year. The server writes the theme into the page it sends, so the page opens in the right colours with no flash.

- **Theme.** System follows your system's light or dark setting; Light and Dark force one.
- **Developer mode.** Off by default. On, the top bar gets a Developer menu with Releases and Definitions; each card and tile names the id of the numbers it shows; Quality gains a card on how well memory is ranked and per-path charts of what became of each memory shown; Cost gains the extraction per session; a memory's page shows its learned ranking as a number; and the CLI commands carry `--dev`.
- **Auto-refresh.** Off by default. On ("Every minute"), the page reloads itself every minute. The Refresh link in the top bar reloads it once. A page left open for more than 5 minutes shows "Updated over 5 min ago. Refresh."
- **Density.** Comfortable by default; Compact fits more on a screen, and touch targets keep their size.
- **Keyboard shortcuts.** Off by default (see Keyboard shortcuts).

The cookie still works if it was written by an older backsight, which kept fewer choices. The dashboard changes memory in no way: a preference only changes what you see.

## Keyboard and screen readers

- **Skip to content.** The first Tab stop on every page is "Skip to content", which jumps over the top bar.
- **Tab order.** On the Overview it runs: Skip to content, the logo, the pages (Overview, Quality, Cost, Bank, Pipeline, Browse, System), the window picker (24 h, 7 d, 30 d, 90 d), the project filter, Refresh, Preferences, then the verdict's info and CLI buttons, and the cards from top to bottom. Enter or Space opens a menu.
- **Charts have a table.** Every chart card has a "Show as a table" control with the same numbers as text, which a screen reader can walk. It needs no script. On a window over 200 days a chart shows no per-day values on hover, and its table lists the latest 120 days. The small trends in the Overview's tiles have a spoken summary instead.
- **Copy buttons need scripts.** Commands the page suggests have a Copy button. Without scripts the button is absent and the command is plain text you can select. With scripts, if the browser does not let the page use the clipboard, the button selects the command and says "Selected: press Ctrl+C".
- **Works without scripts.** Everything else, including the menus, the preferences and the table views, works with scripts off.
- **Contrast and colours.** Text has at least 4.5:1 contrast in the light and the dark theme, and the colours that carry a meaning in charts (used, ignored, a path's state) have at least 3:1. The shades of the stacked bars do not, so a legend and the table behind each chart say the same. Status is never only a colour: it is also a word or an icon. In a high-contrast mode (forced colours) the page's text, borders and focus follow your system's colours; charts keep their own colours, with an outline in the system's text colour.
- **Small screens.** Under 900 px wide, most buttons and links are at least 40 px tall, and the window and project choices move into the Menu button.

## What each view answers

The pages' titles above are the short form of these questions.

| Command | Question it answers |
|---|---|
| `backsight stats overview` | Is memory working, and what needs my attention? |
| `backsight stats quality` | Does memory show up, and is what it shows right? |
| `backsight stats cost` | What does memory cost in context, time and server tokens? |
| `backsight stats bank` | How big is the memory, and what should I review or forget? |
| `backsight stats pipeline` | Is what I write reaching the memory? |
| `backsight stats system` | Are the checks, the server and the background jobs healthy? |
| `backsight stats releases [--compare A,B]` | Did a new backsight version change anything? |
| `backsight stats projects [<name>]` | Which projects use memory, and how well? |
| `backsight stats sessions [<id>] [--has never_extracted\|contradicted]` | Which sessions got memory, which were sent and gave none, and what happened in one? |
| `backsight stats memories [<id>]` | Which memories are shown, used, reported or never used? |
| `backsight stats days [<date>]` | What happened each day? |
| `backsight stats defs [<id>]` | What does a number mean, and when is it judged? |

Flags go after the view word: `backsight stats overview --days 30`, `--project <name>`, `--since` and `--until`, `--tz`. [commands.md](commands.md) lists them all.

`backsight stats releases --compare A,B` (with `--measure used.turn_rate`, `recall.hit_rate` or `verdict.wrong_rate`) compares two versions. It matches turns by project and by where memory was first shown; a turn with no main-agent show falls in the `unknown` stratum. Only groups where both versions have 7 active days and 30 turns count, and the figure it gives is the matched difference with its 95 % interval, read as `differs`, `no clear difference` or `too few to compare`. The comparison is observational: the weeks, the projects and the settings differ between two versions, so it never says one is better. When the unmatched difference points the other way it says so and tells you to read the matched figure.

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
- **A week that is not complete.** In the memory yield card, a week that is not yet complete shows no count of new memories and reads "not complete until" the day it can be judged.
- **Days without a record.** A chart of days past the server's reach reads this PC's daily record. A day neither holds is shaded and left empty, never counted as zero.
- **Memory yield needs four weeks.** The share of new memories used within 28 days is shown for a week only 28 days after it ends, and a week with fewer than 20 new memories shows its counts without a share.
- **Time to recall needs a few days of history.** It starts from the day the daily upkeep began to record, and reads "too few" until 20 sessions have gone the whole way.
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
| `recall.empty_reasons` | Why prompts got nothing | Recalls that injected nothing, counted by reason (the search failed, no candidates, below the cut-off, all already seen, capped), and prompts skipped, counted by reason. "unknown" (shown as "reason not recorded") is a recall logged before version 0.1.19 (31 October 2026), which did not record why; it shrinks as those days leave the window. | count | shown, not judged | 0 | this PC |  |
| `recall.by_lang` | Prompts that got memory, by language | The share of prompt recalls that injected a memory, split by the language of the prompt. Each language needs its own 20 prompts before it is judged. | share | watch when a language is more than 15 points away from the most used one, by more than chance explains | 20 | this PC | yes (`--dev`) |
| `search.rate` | Prompts where Claude searched memory | Typed prompts of at least 40 characters whose turn used memory_search, memory_read or memory_store (main agent), out of all typed prompts of at least 40 characters. | share | watch when clearly below 70% of its usual level (the median of the four previous weeks, once two of them have enough data) | 20 | this PC |  |
| `search.empty_rate` | Searches that found nothing | memory_search calls that succeeded with no hit, out of memory_search calls that succeeded. Failed calls are counted apart. Beside it, from the calls that recorded them: the queries asked; the queries none of whose hits were returned (a query that found memories, all of them ranked out by the other queries' or under the cut-off, counts here: it is not a query that found nothing); and the characters a search answer adds to Claude's context, on average. | share | watch when clearly above its usual level (the median of the four previous weeks, once two of them have enough data) | 20 | this PC |  |
| `used.turn_rate` | Turns where Claude used memory | Main-agent turns that showed memory (the briefing and the similar memory of a memory_store answer do not count) and whose final stop declared at least one memory used, out of those turns that declared (a turn declaring only confirmed, wrong, outdated or none counts as declared without use). The worst case, counting undeclared turns as unused, is given beside it. Unreliable while the declaring coverage's upper bound is under 80%. | share | watch when clearly below 70% of its usual level (the median of the four previous weeks, once two of them have enough data) | 20 | this PC |  |
| `declare.coverage` | Turns where Claude said what it used | Main-agent turns that showed memory (not counting the briefing and a memory_store answer's similar memory) and ended with a declaration, out of those that ended declared or undeclared. Turns whose check was skipped are counted apart, and turns still open are pending. | share | watch when even the upper end of the interval is under 80% | 20 | this PC |  |
| `used.rubber_stamp` | Turns where every memory was marked used | Declared turns where every shown memory was credited, out of declared turns that showed at least two memories. Watched when even the interval's lower bound is above 80%. | share | watch when the lower end of the interval is over 80% | 30 | this PC | yes (`--dev`) |
| `used.share_confident` | Confidently ranked memories that were used | Settled shows credited as used or confirmed, out of shows credited or confidently missed, for the prompt path by default. Shows with an unknown outcome, shows of memories forgotten since, and shows still pending are reported beside it. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `used.rank` | Position of the memories used | How the credited shows were ranked (1 to 5, 6 and beyond, unranked), and how often a used memory was the first, or among the first three, in turns with a credit. | count | shown, not judged | 20 | this PC | yes (`--dev`) |
| `used.by_path` | Used, by where memory was shown | The share of confidently ranked memories that were used, for each way memory is shown (prompt, search and others). Each path needs its own 20 shows. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `search.rescue_rate` | Memories Claude found itself | Credits on the search path in turns where the prompt recall also showed memories, out of all credits in those turns. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `verdict.wrong_rate` | Memories reported wrong or out of date | Wrong and out-of-date verdicts, out of used, confirmed, wrong and out-of-date verdicts. Watched when even the interval's lower bound is above 5%, bad above 15%. | share | watch when the lower end of the interval is over 5%, bad over 15% | 20 | this PC |  |
| `stale.never_used` | Often shown, never used | The memories shown often without ever being used, counted from the feedback ledger with recent feedback weighted more. | count | shown, not judged | 0 | this PC |  |
| `stale.hidden` | Hidden | The memories hidden from recall, counted from the feedback ledger with recent feedback weighted more. | count | shown, not judged | 0 | this PC |  |
| `stale.reported` | Reported wrong | The memories Claude reported wrong or out of date, counted from the feedback ledger with recent feedback weighted more. | count | shown, not judged | 0 | this PC |  |
| `context.per_session` | Context added per session | Characters backsight added to Claude's context per session (prompt recall plus briefing), typical (median) and high (slowest 1 in 20), with approximate tokens (characters divided by 4). Watched when the high value passes 1.5 times the session budget. | chars | watch when the slowest 1 in 20 is over 1.5 times the configured session budget | 100 | this PC |  |
| `context.per_prompt` | Context added per prompt that got memory | Characters injected by prompt recalls that put memory in front of Claude. The value is the typical (median) size; the high value (slowest 1 in 20) is given beside it once there are 100 recalls. | chars | shown, not judged | 20 | this PC |  |
| `recall.latency` | Time memory adds to a prompt | Wall time of the prompt recall, typical (median) and slowest 1 in 20. Watched above 2,000 ms, bad above 2,500 ms; when the last 24 h (20 recalls or more) is back at or under 2,000 ms, bad reads watch, marked improving. | milliseconds | watch when the slowest 1 in 20 is over 2000 ms, bad over 2500 ms | 100 | this PC |  |
| `search.latency` | Time of a memory search | Time of a memory_search call, typical (median) and slowest 1 in 20. Watched above 3,000 ms. | milliseconds | watch when the slowest 1 in 20 is over 3000 ms | 100 | this PC |  |
| `extraction.tokens` | What the server's model spent | Tokens (input, of which cached, and output; the total is input plus output plus thoughts, which count with the output, so it is a little above the server's own total) the server's model spent per day and per operation, for all PCs using the server. The server keeps 30 days; older days come from the days this PC recorded, and a day neither holds is left empty. | tokens | shown, not judged | 0 | server (all PCs) |  |
| `extraction.cost` | What the server's model spent | The tokens spent, priced from the [llm] prices in your backsight config; it reads "price not set" when no price is set. For all PCs using the server. The server keeps 30 days; older days come from the days this PC recorded, and a day neither holds is left empty. | money | shown, not judged | 0 | server (all PCs) |  |
| `extraction.per_session` | Extraction per session sent | Extraction tokens of the last 28 days divided by the sessions sent successfully in those days, as of now whatever the window. The tokens are every PC's on the server; the sessions are this PC's, so on a shared server the figure is too high. | tokens | shown, not judged | 0 | server (all PCs) | yes (`--dev`) |
| `bank.size` | Memories | Memories by kind (facts about you and your work, facts from sessions, summaries) and the documents they come from. | count | shown, not judged | 0 | server (all PCs) |  |
| `bank.growth` | New memories per day | Memories added per day, by kind, from the server's time series (90 days); older days come from the days this PC recorded, and a day neither holds is left empty. | count | shown, not judged | 0 | server (all PCs) |  |
| `bank.by_project` | Memories per project | Memories per project, from the server's project tags. | count | shown, not judged | 0 | server (all PCs) |  |
| `bank.consolidation` | Summaries waiting | Summaries pending and failed, and when the last one was built. The failed count is the server's current total. Watched when any failed. | count | see What it counts | 0 | server (all PCs) |  |
| `bank.churn` | Retired, hidden, restored | Forget, retire, hide and restore actions recorded per week. | count | shown, not judged | 0 | this PC |  |
| `notes.stored` | Facts stored | Store events by result (saved, updated, refused) and by kind. | count | shown, not judged | 0 | this PC |  |
| `imports.sent` | Files imported | Files sent from the folders given to backsight import, with the import events by action (queued, sent, paused at the cost cap, finished, removed). | count | shown, not judged | 0 | this PC |  |
| `pipeline.flow` | Sessions sent | Documents whose last status is ok, out of documents with a send in the window. The interval counts documents (a session's documents per project), not independent sends. Failed, set aside and waiting ones are given beside it. A note logged queued with nothing after it waits only while the notes outbox still holds it (queued at or after its oldest note); one that left the outbox counts as sent. Watched when any failed or was set aside; bad when the backlog is older than 24 hours. | share | see What it counts | 0 | this PC |  |
| `retain.outcomes` | Sends by result | Per day, documents by their last status, a queued note that left the notes outbox counted as sent and a send logged without a status as unknown. The duration (slowest 1 in 20) counts successful sends only. | count | shown, not judged | 0 | this PC |  |
| `local.backlog` | Waiting on this PC | Notes queued, sessions waiting to be sent, items set aside, and the age of the oldest (a note logged queued counts only while the notes outbox holds it). Watched when the oldest is over 1 hour, bad over 24 hours. | count | see What it counts | 0 | this PC |  |
| `server.queue` | Waiting on the server | Operations queued and running on the server, from its operations list. The failed count covers operations updated in the last 7 days, whatever the window; when the server holds more failed operations than it lists, the count is a lower bound. Watched when any failed. | count | see What it counts | 0 | server (all PCs) |  |
| `server.extraction_delay` | Time to turn a session into memories | Time between a completed operation's creation and its last update, typical (median) and slowest 1 in 20, over the last 200 in the window. Watched above 30 minutes. | milliseconds | watch when the slowest 1 in 20 is over 30 min | 100 | server (all PCs) |  |
| `hooks.failures` | Steps that crashed | Hook failures by kind (panic, fatal, timeout, killed, exit, invalid output, input too large, spawn). In the last 24 hours, a panic, fatal, exit or spawn failure, or any kind three times, is watched; ten or more is bad. | count | see What it counts | 0 | this PC |  |
| `server.reach` | Memory server answering | The last answer, since when it is failing, and until when recall stays paused after repeated failures, as this PC recorded them, with the prompts skipped while it was paused. Watched when failing or while recall is paused; bad when a backlog older than 24 hours builds up with it. | count | see What it counts | 0 | this PC |  |
| `doctor.check` | Health checks | The doctor's checks, one entry per check with its status. Watched when any check is an error. | count | see What it counts | 0 | this PC |  |
| `jobs.last_run` | Background jobs | The last tick and maintain runs, with status and duration. Bad when a job is more than twice its cadence late and prompts or tool events happened since it was due (a sleeping laptop is not a fault). | milliseconds | see What it counts | 0 | this PC |  |
| `pipeline.time_to_recall` | From session to recalled memory | For each session of this PC, the time from its last prompt to its first memory shown in a later session, split into three steps: sent, turned into memories, shown again. The value is the typical (median) whole way; the slowest 1 in 20 of the sent-to-memories step needs 100 sessions and is watched above 1 hour. | milliseconds | watch when the slowest 1 in 20 is over 60 min | 20 | this PC |  |
| `memory.yield` | Memories that proved useful | Per week, the share of the memories created that week which Claude used within 28 days of their day, with the tokens the server spent per useful memory. Memories come from the server, uses from this PC, so with several PCs the share reads low. | share | shown, not judged | 20 | this PC |  |
| `secrets.suspects` | Possible secrets in memory | The memories the last scan found that look like a key, token or password. Watched as soon as the last scan found one. | count | see What it counts | 0 | server (all PCs) |  |
| `doctor.history` | Health checks over time | For each check and each day, the result of the day's last daily run: ok, watch or bad. A day without a run is shown as none, which is not a fault. | count | shown, not judged | 0 | this PC |  |
| `bank.active_share` | Share of the memory that came up | The memories shown in the window that already existed when it began, out of all the memories the server held then. Memories created inside the window are left out. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `shows.concentration` | How concentrated shows are | The Gini index of how often each shown memory was shown in the window: 0 when all are shown equally, near 1 when a few take almost every show. The share of the 10 most shown is given beside it. Every kind of show counts, answers from sources included. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `injection.contradicted` | Corrections after memory was shown | The share of turns with injected memories where the next prompt looked like a correction. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `boost.set_changed` | Recalls the learned ranking changed | The share of recalls, prompts and searches, whose shown memories differ from what the ranking would show without what it learned from past feedback. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
| `stopcheck.block_yield` | Held turns that then credited a memory | Turns the end-of-turn check held, and whose agent then credited a memory used or confirmed, out of the held turns that ended again. Beside it: the held turns by what followed (used, confirmed, corrected for wrong or outdated, none, refused for no answer), and by where the memories were shown when the check held them (prompt, search, briefing and others; a turn with two counts in each, and each needs its own 20 turns). Turns held before this was recorded are not counted. | share | shown, not judged | 20 | this PC | yes (`--dev`) |
<!-- catalog:end -->

## For agents

An agent answers "is memory working?" with `backsight stats overview --json`, then drills down by view. The [agent guide](agent-guide.md#is-memory-working) says which fields to read.
