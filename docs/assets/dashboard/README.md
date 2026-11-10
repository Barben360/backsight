# Dashboard screenshots

Five pages in both themes at 1440 x 900, drawn by the real renderer from the test fixtures, not from anyone's memory: the Overview healthy, learning (a new install) and with the server down, a session and a memory. The README shows `overview-healthy-*.png`.

The command below also writes four taller pages in both themes (`overview-full`, `pipeline`, `system`, `releases-compare`), which are not kept here: delete them after the run, or point `BACKSIGHT_SCREENSHOTS` at another directory.

To take them again from the repository root, with Chrome or Chromium on the `PATH`:

```sh
BACKSIGHT_SCREENSHOTS="$PWD/docs/assets/dashboard" go test ./internal/dashboard -run TestScreenshots -count=1
```
