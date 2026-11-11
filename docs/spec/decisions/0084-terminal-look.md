# 0084 — A colorful terminal: a splash on every command and colored text, for people only

- **Date**: 2026-11-09
- **Status**: accepted (v2, after a builder check and a code-feasibility review by two Sonnet 5.5 reviewers); built 2026-11-10 (backsight PRs https://github.com/Barben360/backsight/pull/264, https://github.com/Barben360/backsight/pull/266, https://github.com/Barben360/backsight/pull/265, https://github.com/Barben360/backsight/pull/267; release v0.1.35-m7). The differences from the text below are listed in the "As built" section at the end and in `decisions/INDEX.md`.
- **Decided by**: author (2026-11-09: "Backsight output when running terminal commands is a bit 'bleak'. I'd like it to be more colorful, with some 'splash screen' showing stylized ascii-art logo (like in fastfetch or neofetch for instance). Of course, the parsable output should still exist, for automating […], and tools relying on that should keep working correctly." · "The splash shows every run for all commands (backsight update check/apply, backsight version, backsight doctor... etc), but only for terminal as you proposed." · "For the info panel, put what you think most pertinent. I'd put no current project though, only 'backsight-generic' stuff." · "Ok for me. I approve."), on the agent's reading of backsight at 4bdc04d1
- **Amends**: SPEC §11.2 (command contract: text output gains color and a splash at an interactive terminal; the JSON envelope and exit codes are unchanged)

## Context
- Every command prints plain text, or one JSON envelope with `--json` (`internal/output`, schema_version 1, fixed exit codes). In `--json` mode progress goes to stderr as JSON lines.
- Text reaches stdout two ways: `env.emit` / `env.report` (117 call sites, `internal/cli/cli.go`), and about 32 direct writes to `e.stdout` (the `stepLine` streams of `server`, `connect`, `restore`, `update apply`; `setup`'s plans and prompts; `server ssh install`; `uninstall`; `backup`; `server token`). About 22 more write to `e.stderr` directly (progress, prompts, the final `backsight: <error>` line at `cli.go:78` and `cli.go:210`).
- **Claude reads the plain text**: the doctor and memory skills run `backsight stats`, `stats index`, `doctor` and others without `--json` through Claude Code's Bash tool, whose stdout is a pipe (checked in this session) and whose environment carries `CLAUDECODE=1`. Golden tests compare text byte for byte.
- `hook` and `hook-child` return before cobra (`cli.go:166-172`); `mcp` speaks JSON-RPC on stdio. The hook banner already paints the brand word rosewood (`#C9507A`, bold) in the `systemMessage` Claude Code renders to the user, unless `NO_COLOR` is set (`hook_env.go:173`); that is unchanged.
- **Windows scheduled jobs run under `conhost --headless` with no redirect** (`jobs_log.go:41-60`): their stdout is a console, so a terminal check alone would say "terminal"; `jobLogged` tees stdout and stderr into the job log with `io.MultiWriter`.
- The existing `stdoutIsTerminal` reads `os.Stdout.Fd()`, not the writer `ExecuteContext` was given (tests pass buffers).
- `guide` pipes its text to `$PAGER` (default `less -FRX`) at a terminal (`guide.go:151`).
- The logo (`docs/assets/logo-*.svg`): three concentric arcs opening right, rosewood `#B4436C` at opacity 1, 0.6 and 0.3 (inner to outer), and an orange dot `#E8913A` at their center.

## Goals
- At an interactive terminal, every command people run starts with a neofetch-style splash: the logo in ASCII art beside a short panel about backsight itself (never the project).
- At an interactive terminal, text output is colored where it helps reading.
- Every other consumer gets exactly the bytes it gets today.

## Non-goals
- A new machine format (jsonl or other): `--json` stays the machine contract, unchanged.
- Any change of wording, order or layout of text output: color is added around existing characters, never instead of them.
- A full-screen TUI; color in the web dashboard; any change to the hook banner.

## Anti-goals (a failure even if it works)
- An escape sequence or splash line reaching Claude (Bash tool, `!`, hooks, MCP), a pipe, a redirect, a scheduled job or its log, a `--json` consumer, a value meant to be captured (a token, a key), or a golden test.
- The splash failing, blocking or visibly slowing a command, or creating any file or directory.
- Color carrying meaning alone: every colored status keeps its word.

## Decision

### D1 — When color is on
Resolved **once per stream** (stdout, stderr), after cobra has parsed the flags, first match wins:
1. `--json` (as parsed, including `--json=true`) → **off** for both streams, whatever else is set.
2. The command is marked **plain** (D2) → **off**.
3. `--color=always|never|auto` (new global flag; default `auto`; honoured like `--json` when parsing fails).
4. `BACKSIGHT_COLOR=always|never|auto`.
5. `NO_COLOR` non-empty → **off** (no-color.org).
6. `auto` → on only when all hold: the stream **writer** is an `*os.File` whose descriptor is a terminal (a buffer or any other writer is never a terminal; a test seam on `env` fakes one), `TERM` is not `dumb`, and `CLAUDECODE` is not set (belt and braces: even a wrapper that gives Claude's shell a pseudo-terminal gets plain text).

Generic forcing variables (`FORCE_COLOR`, `CLICOLOR_FORCE`) are **ignored**: a user who exports one in a shell profile would otherwise send escapes into Claude's Bash calls, which inherit that environment.

Color levels: truecolor when `COLORTERM` is `truecolor` or `24bit`; else 256 colors when `TERM` contains `256color`; else the 16 basic colors.

**Windows**: color needs virtual-terminal processing on the console handle (`ENABLE_VIRTUAL_TERMINAL_PROCESSING` through `golang.org/x/sys/windows`), set per handle (stdout, stderr). The previous mode is saved and restored on every return path of `ExecuteContext`, including Ctrl-C cancellation (`main` calls `os.Exit`, which skips defers). If setting it fails, that stream is off. Mintty/Git Bash (a pipe, not a console) resolves to off under `auto`.

### D2 — Which commands: splash, color, plain
Every command carries one of three cobra annotations; a test walks the whole command tree (including hidden commands and cobra's `completion` / `__complete`) and **fails on any command without one**, so a new command must be classified:

| Class | Splash | Color | Commands |
|---|---|---|---|
| **plain** | no | no | `hook`, `hook-child`, `mcp`, `completion`, `__complete`, `jobs` and all its subcommands (Windows runs them on a headless console, and their output goes to the job log), `secret` and its subcommands, `server token`, `backup identity show`, `recall-prompt`, `detect-replay`, `install-shim`: commands run by programs or printing a value meant to be captured |
| **no-splash** | no | yes | `guide` (its text goes to a pager) |
| **full** | yes | yes | every other command, including `version`, `doctor`, `update check`, `update apply`, `stats` and its views, `setup`, `connect`, `uninstall`, `dashboard` |

The builder may move a command to a stricter class if its audit finds it feeds a program; never to a looser one without the author.

### D3 — When the splash prints
- **Lazily, on the first byte written to stdout** by a **full** command whose stdout color is on (D1), and that stdout is a terminal (even `--color=always` does not print a splash into a pipe). The splash writer wraps `e.stdout`; one flag per process ensures it prints once.
- This gives the D2 rules without per-command code: cobra help (`--help`, `help`, bare `backsight`) writes to stdout, so it gets the splash; usage errors and runtime failures write only `backsight: <error>` to stderr, so they get none and the error comes first; bare groups (`backsight server`) are usage errors, so none.
- The terminal must be at least 72 columns wide; if the width query fails or is narrower, no splash and the output is unchanged.
- **Interleaving**: progress written to stderr before the first stdout byte appears above the splash. Accepted: these are the slow first runs (`stats overview` building the index); the splash is not delayed for them.
- **Copies of `env`** (`server_ssh_install.go:510` builds a second `env` from `e.stdout`): the splash state and color live in the writers, so a copy that reuses `e.stdout` shares them.
- **Windows jobs log**: jobs are plain (D2), so `jobLogged`'s tee never sees an escape or a splash.

### D4 — What the splash shows
Left: the logo in about 7 rows. Inner arc in full rosewood, middle arc dimmer, outer dimmest (fixed shades standing for the SVG's opacities, chosen to read on dark and light backgrounds), the dot in orange. Unicode block characters; a pure-ASCII variant when the locale is not UTF-8 (`LC_ALL` / `LC_CTYPE` / `LANG` on Unix, `GetConsoleOutputCP` ≠ 65001 on Windows).

Right: a panel about backsight itself, **never the current project**. Each line is omitted when its source is missing, unreadable, over its size cap, or late (below):

| Line | Content | Source | Color |
|---|---|---|---|
| title | `backsight <version> · Hindsight <tag>` | build info; the tag parsed from `server.HindsightImage` (an image reference such as `ghcr.io/…/hindsight:1.2.0@sha256:…`) | brand word bold rosewood |
| Server | `answering · last answer 2m ago`, `silent since <time> (<age>)`, `another server answers at <host>`; on a client, prefixed by the server's host | `reach.json`, `breaker.json`, `state.json` (the rules of `stats`'s Server line); omitted when `reach.json` is absent (a server-only host has no hooks) | green / amber / red |
| Setup | `host · docker` / `host · native` (role `both`, mode `local`), `client of <host>` (mode `remote`), `server only · <runtime>` (role `server`); `not set up yet · backsight setup` without a config | `config.toml` (role, mode, runtime, server URL host) | plain; the command cyan |
| Queue | `<n> sessions · <n> notes waiting` (`1000+` past the cap) | `reach.json`'s `sessions_waiting`; the outbox's line count | dim when both 0, amber otherwise |
| Checks | `<n> ok · <n> warn · <n> fail (last run <age> ago)` | `doctor-snapshot.json` (written by `jobs maintain`): counts from its checks' statuses | per count |
| Update | `<version> available · backsight update apply`, only when newer than the running binary | `state.json`'s update check, `updateBanner`'s rule (`release.NewerThan`) | orange |

Indicative layout (the exact art is the design sub-issue's, checked on dark and light themes):

```
     ▄▄▀▀▀            backsight 0.1.33-m7 · Hindsight 1.2.0
   ▄▀   ▄▄▀▀          ──────────────────────────────────────
  █   ▄▀   ▄▀▀        Server   answering · last answer 2m ago
  █   █   █    ●      Setup    host · docker
  █   ▀▄   ▀▄▄        Queue    3 sessions · 0 notes waiting
   ▀▄   ▀▀▄▄          Checks   24 ok · 1 warn (last run 19h ago)
     ▀▀▄▄▄            Update   0.1.34-m7 available · backsight update apply

<the command's output>
```

Rules for the panel:
- **Dedicated read-only readers**, not the existing helpers: `config.Load` validates the whole config, `Outbox.Pending` decodes every outbox line with a 64 MiB buffer, and `reachOf` loads the config again. The splash readers use `paths.Default()` (never `machine()`, which runs `EnsurePrivate`), read each file with a cap (64 KiB; 1 MiB for `doctor-snapshot.json`), count outbox lines without decoding them (up to 1,000), take no lock, and write nothing.
- **Budget**: the panel is built under a 50 ms deadline; a reader that misses it drops its line. A benchmark measures it; no test gates on a wall-clock time (a Windows CI page-budget test already flaked on 2026-10-31).
- **Never fails a command**: a panic or error in the splash drops it silently.
- Values pass through `maskTokenText` like any text output.

### D5 — Colored text
- **Style API** (one package, e.g. `internal/termstyle`): roles, not colors, and a no-op when the stream's color is off:

| Role | Color |
|---|---|
| brand | rosewood `#C9507A` bold |
| accent | orange `#E8913A` |
| ok | green |
| warn | amber |
| fail / error | red |
| label | bold |
| secondary (ids, paths, times) | dim |
| command to run, URL | cyan |

- **Commands style their own output** through this API, at the places that build the text (emit/report text functions, `stepLine`, plans, prompts). There is **no regex pass over all output**: a whole-word or URL pattern can match inside a token, a path or a JSON-looking line. The builder may add a narrow generic rule (a label column anchored at line start) applied only to commands that opt in.
- **Order of styling**: the command first marks roles, `maskTokenText` still runs on the text and must not break on escapes (the builder either masks before styling or masks the unstyled text and styles the masked result; the tests below decide).
- **Never styled**: passphrase and secret prompts (`backup.go:458`, `secret.go:153`), and everything in a plain command.
- **Invariant**: removing SGR sequences (`\x1b\[[0-9;]*m`) from the colored text gives back exactly the plain text, byte for byte.
- **First commands styled** (phase 3): `doctor`, `version`, `update check`, `update apply`, `stats` and its views, then the `setup` / `connect` / `uninstall` plans, step lines and prompts, then the stderr `backsight: <error>` prefix (red) and text-mode progress lines. Commands not yet styled print as today (and still get the splash).

### D6 — Machine output is unchanged
- `--json`: same envelope, same bytes, no splash, no escape, even with `--color=always` or `BACKSIGHT_COLOR=always`.
- Exit codes unchanged; the splash never changes one.
- Plain commands print exactly as today, whatever the new flags and variables say.
- Skills need no change. `docs/agent-guide.md` gains one line: plain text and `--json` are unchanged off a terminal, and `CLAUDECODE` turns color off.

### D7 — Documentation
- `docs/commands.md`: the global flags `--color` and `--no-splash`; the three command classes and their lists.
- `docs/configuration.md`: `BACKSIGHT_COLOR`, `BACKSIGHT_SPLASH`, `NO_COLOR`, `CLAUDECODE`, and why `FORCE_COLOR` is ignored.
- `README.md`: a screenshot of the splash (optional).
- Release notes per the release convention.

### D8 — Opting out of the splash only
`--no-splash` (global flag) or `BACKSIGHT_SPLASH=0` hides the splash and leaves color as resolved by D1.

## Tests that must bite
- **Off a terminal, unchanged**: run the whole existing CLI suite with the real descriptors possibly being a terminal (`go test` at a terminal): no `\x1b` byte and no splash line in any captured stdout/stderr. Fails if detection reads `os.Stdout` instead of the writer.
- **Invariant**: every golden text rendered with color forced on equals the plain golden after SGR stripping; a fuzz test over styled outputs with random data (tokens, paths, URLs) holds the same.
- **`--json`**: with `--color=always`, `BACKSIGHT_COLOR=always`, `--json=true` and a faked terminal, the envelope bytes equal those without them.
- **Resolution**: each rule of D1 wins over the next; `TERM=dumb`; `CLAUDECODE=1` turns `auto` off but not `--color=always`; `FORCE_COLOR` ignored.
- **Classification**: a command added without a class annotation fails the tree-walk test; each plain command, with a faked terminal and `--color=always`, prints no escape and no splash.
- **Splash**: printed once with a faked 80-column terminal, before the first stdout byte, on a command and on `--help`; absent at 71 columns, when the width query fails, with `--no-splash`, `BACKSIGHT_SPLASH=0`, `NO_COLOR`, `--json`, on a usage error (`backsight server`, an unknown flag), on a runtime failure that prints only to stderr, for `guide`, and on a copied `env`.
- **Panel**: each line from fixture files; a missing, corrupt or oversized file drops only its line; a slow reader (faked) drops its line at the deadline; a fresh machine (no config, no state directory) shows `not set up yet` and **creates nothing on disk**; no project name appears even when run inside a project directory.
- **Masking**: a connection token in a styled line is still masked.
- **Windows**: VT enable failing turns that stream off; the console mode is restored after the command, including after cancellation (faked handle).

## Delivery
Epic: backsight issue 257 (https://github.com/Barben360/backsight/issues/257), sub-issues 258 (design), 259 (phase 1), 260 (phase 2), 261 (phase 3), 262 (phase 4). One PR each:
0. **UI/UX design** (Claude Design): the logo art and its ASCII variant, the shades on dark and light themes, the panel wording. Before phase 2.
1. **Style layer and resolution** (D1, D2, D6): the flags and variables, writer-based detection, the class annotations and their tree-walk test, Windows VT, the off-terminal and `--json` tests.
2. **Splash** (D3, D4): the lazy writer, the panel readers, the tests.
3. **Colored text** (D5): the commands in the listed order.
4. **Docs and release** (D7).

## Left to the builder
- The exact art and ASCII variant (after the design sub-issue), the exact shades and their 256/16-color fallbacks.
- The style package's name and API, within D5's roles.
- How `maskTokenText` and styling compose, provided the masking test passes.
- Moving a command to a stricter class after auditing it (D2).

## As built (2026-11-10)
- **D1 order** (author, 2026-11-09, choice "A"): `CLAUDECODE` is checked right after `--color` and before `BACKSIGHT_COLOR`, so only `--color=always` overrides it; a `BACKSIGHT_COLOR=always` exported in a profile never reaches Claude's shell. `WT_SESSION` also means truecolor.
- **D2**: `server supervise` and `server native-check` are plain too (their output feeds a program); `install-shim` is the hidden `__install-shim`.
- **D4 caps**: 64 KiB for `config.toml` and `breaker.json`; 1 MiB for `reach.json` (one id per waiting session) and `doctor-snapshot.json`; 4 MiB for `state.json` (its tombstones grow without bound: 160 KB on the author's PC); the outbox is counted to 1,000 lines or 8 MiB. A server mismatch shows only in mode `remote`, as in `stats`. Checks counts `info` as ok and `error` as fail, and always shows the three counts. In ASCII mode the panel's punctuation is ASCII too.
- **Logo**: the half-block art (A) by default; the braille art (B) with `[terminal] logo = "braille"` (author, 2026-11-09: "Let's use A but let B be available through a config variable"); unknown text draws blocks, a value that is not text is refused like any key. The ASCII art (D) on a console that is not UTF-8. Shades as approved in backsight issue 258, the text palette darkened to read on light themes too ("After."). **Changed in v0.1.36-m7** (author, 2026-11-10: "I actually prefer the braille version. Make it default"): the braille art (B) is the default, the half blocks (A) with `[terminal] logo = "blocks"`, unknown text draws braille (backsight PR https://github.com/Barben360/backsight/pull/268).
- **D5**: the views' tabwriter tables and section headings, the y/N confirms, uninstall's "type the bank id" prompt and `setup --undo`'s list stay plain (escapes inside tabwriter cells break the columns).
- **Windows**: the code-page rule is kept as written (ASCII logo unless 65001), although Go writes UTF-8 to a console through WriteConsoleW whatever the code page; to revisit with the author.

