# backsight — specification and decision record

The specification of **backsight** and every decision behind it: why the binary does what it does, the options weighed, and what was built. The code cites these records by number (for example `decision 0069`); this directory is where they live. It is for people working on backsight. The user documentation is the rest of [docs/](..), and none of this directory is embedded in the binary.

## Layout

| Path | Role |
|---|---|
| `SPEC.md` | The living specification, written from the recorded decisions. |
| `decisions/NNNN-<topic>.md` | One decision per file: context, options, choice, why, date. |
| `decisions/INDEX.md` | Which later decisions amend or supersede each one, and the "as built" records. |
| `ROADMAP.md` | Milestones, in order. |
| `OPEN.md` | Questions still open, in dependency order. |
| `research/` | The checks done before the specification (facts, sources, benchmarks). |
| `experiments/` | The experiments that settled a decision, and their results. |

## Rules

- **Spec first.** New work starts with a decision file, then an issue, then a plan, then the code. A change that alters behaviour updates `SPEC.md`.
- **Decisions are never rewritten.** A changed mind is a new decision that supersedes the old one, recorded in `decisions/INDEX.md`. Typos and broken links may be fixed in place.
- **"As built" records.** Once code ships, the decision's INDEX row says what was built, where (pull requests, release) and how it differs from the decision.
- Decisions go through pull requests like code. A pull request that changes only Markdown here skips the code jobs of CI (`scripts/ci-docs-only.sh`).
- Anyone may propose a decision: open a pull request with a new `decisions/NNNN-<topic>.md` (the next free number, status `proposed`) and the issue it answers. The maintainer accepts, amends or declines it.

## Credo

**Everything that can be deterministic stays deterministic.** An LLM (an agent installing, updating or repairing) only makes contextual decisions and always acts through deterministic commands of the binary. See [decisions/0008-deterministic-first.md](decisions/0008-deterministic-first.md).

## History

Until 2026-11-12 this record lived in a separate private repository (decision 0006). It moved here so that contributors can read the reasons behind the code and propose changes (decision 0086). Its implementation plans, experiment scripts and raw experiment outputs stayed in that archive, because they hold data from the author's own memory; files here still mention them by path (`plans/…`, `E*-scripts/`, `raw/…`). Names of the author's other projects were replaced with stand-ins (`ref-stack`, `my-garage`, `clinic-app`, `trip-2031`).

The reference implementation was the stack running on the author's Linux workstation (`ref-stack`). The initial request and its review are in [research/00-initial-request-and-review.md](research/00-initial-request-and-review.md).
