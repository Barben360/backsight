# 0068 — The repository goes public for the preview; the author's work PC is the first preview install

- **Date**: 2026-10-25
- **Status**: accepted
- **Decided by**: author (2026-10-25: "For the preview access, let's make it public, no one is tracking it anyway, I'll advertise it later. I'll test it on my work PC. For the rest, I follow your recommendations."), on the agent's questions after 0067

## Decision
- **Public at the end of M5**: `Barben360/backsight` becomes public once M5's blocks and review are done, right before the preview. This supersedes 0063 Q2's "private until M7". The step before it is a gitleaks scan of the whole history, not just the working tree; a finding stops the flip and goes to the author.
- **Moved from M7 to the end of M5**, because a public repository makes them work:
  - a signed pre-release tag;
  - `install.sh` and the agent guide at stable URLs;
  - `update check` reading public releases without a token.
- **Stays in M7**: macOS and Windows validation, the first stable release (not a pre-release), the Windows scheduler wrapper and the minimum version in `install.sh` (0066).
- **Preview** (0048): the author's work PC is the first install, through the published bootstrap. Outside users come when the author advertises the project. 0048's onboarding measures apply to every preview install, the author's included.
  - This brings the work PC back into scope as an install target, without merging it with this machine's memory (0033: separate memories).
  - Two questions are asked at that install, not before:
    - **Q7**: import the work PC's old Hindsight session text?
    - The reference install the work PC already runs (§11.5).
  - If the work PC is not Linux, its install also serves as an early M7 check on that OS.
- **Commands with no milestone yet** (SPEC §17):
  - `verify --live` goes to M5 block 4, so block 5 can use it on the author's machine.
  - `setup --repair` goes to M7.

## Why
A public repository removes the token workarounds that 0067 block 2 needs for updates, and makes the bootstrap and the agent guide reachable the way §11.1 describes. The author says nobody tracks the repository yet, so going public before advertising costs nothing. The whole-history scan guards against publishing an old secret.
