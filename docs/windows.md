# backsight on Windows

backsight runs on Windows 11 x86_64 (amd64), for a standard user: no step asks for administrator rights or writes outside your user profile. Claude Code runs natively (its Git Bash), and the memory server runs as programs of your own user, without Docker.

## Install the binary

In PowerShell (Windows PowerShell 5.1, which Windows ships, or PowerShell 7):

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.ps1 | iex"
```

`-ExecutionPolicy ByPass` applies to that one command: your execution policy is not changed. The script does what `install.sh` does on Linux:

1. finds the newest release, pre-releases included, through GitHub's API (no account or token needed), and downloads its `checksums.txt`, its signature and the Windows archive, `backsight_<version>_windows_amd64.zip`;
2. checks the signature of `checksums.txt` with the release key written in the script, then the archive's SHA-256, and stops if either fails. Windows has no minisign, so the script checks the signature itself, in plain PowerShell (BLAKE2b and Ed25519, under a second): no other program is downloaded or trusted;
3. runs the verified `backsight.exe`, which installs itself as `%LOCALAPPDATA%\backsight\bin\<version>\backsight.exe` with the signed checksums next to it (it checks itself against them on its first run), copies itself to the [shim](glossary.md#shim) `%LOCALAPPDATA%\backsight\bin\current.exe` and to the command `%LOCALAPPDATA%\Programs\backsight\backsight.exe`, and adds `%LOCALAPPDATA%\Programs\backsight` to your user `PATH` (the user's, not the machine's).

A terminal reads `PATH` when it starts: if the script says so, open a new terminal (PowerShell or Git Bash) before running `backsight`. Then:

```powershell
backsight setup
```

With `BACKSIGHT_HOME` set, everything goes under that directory instead (`data`, `config`, `state`, and `bin` for the command), as on Linux.

To install one given release instead of the newest, set its download URL first:

```powershell
$env:BACKSIGHT_RELEASE_URL = 'https://github.com/Barben360/backsight/releases/download/<tag>'
irm https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.ps1 | iex
```

If the script cannot list the releases (no network to `api.github.com`, or GitHub's limit of 60 requests an hour for an address without a token), it stops and names `BACKSIGHT_RELEASE_URL`: set it as above, with a tag from the [releases page](https://github.com/Barben360/backsight/releases).

To install from release files you downloaded yourself (`checksums.txt`, `checksums.txt.minisig` and the zip in one directory), set `$env:BACKSIGHT_RELEASE_DIR` to that directory, then run the script (saved as `install.ps1`: `powershell -ExecutionPolicy ByPass -File install.ps1`). Nothing is downloaded then.

The script refuses, and installs nothing, when:

- the signature does not verify: `checksums.txt: bad signature`, `signed by another key`, `bad trusted comment signature`, `not a prehashed minisign signature` (a release is always signed prehashed), or `was made for another file`;
- the zip is not the one `checksums.txt` lists: `checksum mismatch`;
- the machine is not Windows x86_64: on Windows on Arm, there is no backsight yet; on Linux and macOS, use `install.sh`.

Any of these with a release from GitHub: download nothing more, and report it.
