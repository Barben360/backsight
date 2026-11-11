# 0027 — Releases signed with minisign, two embedded keys; images pinned by digest

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
Binaries and install scripts are verified with minisign (Ed25519); the binary embeds the current and next public keys for rotation. Container images are trusted through the digest pinned in a signed release, no cosign at runtime.

## Why
Simple, offline verification in pure Go (C11); a digest is content-addressed, so the signed release already authenticates the image.
