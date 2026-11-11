# 0070 — A server runtime interface now; a native runtime on Linux, then native by default on macOS and Windows

- **Date**: 2026-10-25
- **Status**: accepted
- **Decided by**: author (2026-10-25: "I approve all the steps"), on the agent's challenge of the author's proposal ("offer a native run on Linux, using systemd … easier for us to enforce native running on both Windows and macOS")
- **Amends**: 0011 (Docker Compose only in v1), SPEC §19 ("native (non-Docker) server" out of v1)

## Decision
- **A. Interface now (M5 block 2)**: the server's Docker-specific operations go behind a server runtime interface, with Docker Compose as its only implementation. That covers install/render, start, stop, status, dump, restore, running on a temporary key, upgrade to a pinned version, a throwaway instance for `backup verify`, and the version identity used by restore. It is built with `update apply` (backsight issue 209). Callers it cannot reach there get a follow-up issue in M5.
- **B. E29, after M5**: Hindsight 1.1.0 runs natively (`hindsight-api-slim[local-onnx,embedded-db]`, embedded pg0) under a systemd user unit on the author's machine, with a throwaway bank. It measures:
  - install time and size, cold and warm start;
  - backup and restore with pg0's own tools;
  - behaviour after a crash or a failed start (the orphaned pg0 of C09);
  - recall parity against Docker on the same data (E2 method).

  It also checks pg0's current upstream status and the data-loss guard that upstream issue #675 led to. **E29 decides whether pg0 is safe enough** to hold the memory.
- **C. Native Linux runtime, if E29 passes (first block of M7)**:
  - `server.runtime = docker | native`. Docker stays the Linux default.
  - Native means a pinned Python through `uv`, a hash-locked dependency set per OS and architecture, pg0, and a supervising systemd user unit.
  - It ships with no web UI unless the Control Plane is added later.
  - Switching runtime is a backup in one and a restore in the other (same `pg_dump` format).
- **D. macOS and Windows (M7)**: native is the default runtime there, through a launchd agent and a Windows per-user service or Task Scheduler entry. Docker stays possible.
- **Unchanged**: M5 ships with Docker only. The author's work PC preview uses Docker: it is installed there and the author administers the machine.

## Why
Docker is the biggest install hurdle on macOS and Windows. Docker Desktop is paid in larger companies, runs a VM, and gives no GPU on macOS (0035). Building the native runtime first on Linux, where systemd user units are already generated for the jobs, rehearses the abstraction on the easiest platform. The interface comes now because M5 is writing the last Docker-specific lifecycle code (update and server upgrade). E29 comes before any native code because upstream labels pg0 "development", and the memory would rest on it.
