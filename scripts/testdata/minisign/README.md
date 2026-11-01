Fixtures of install.ps1's minisign check (install_ps1_test.go):

- `v0.1.18-m7/`: the real release v0.1.18-m7's `checksums.txt` and `checksums.txt.minisig`, signed by backsight's release key (`minisign.pub`).
- `tampered/`: the same signature, over a `checksums.txt` with one hex digit changed.
- `other-key/`: a signature by another key, a throwaway key whose public half is `key.pub` (its secret half was never kept), copied from `internal/signing/testdata/minisign`, `checksums.txt.legacy.minisig` (`minisign -S -l`) included. It is not `minisign.next.pub`'s key, whose secret half these tests cannot have: it stands for any key install.ps1 does not trust.
- `no-file-field/`: a valid prehashed signature by a throwaway key (`key.pub`, secret half discarded) whose trusted comment has no `file:checksums.txt` field (`minisign -S -t 'timestamp:…<TAB>hashed'`).
