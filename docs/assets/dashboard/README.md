# Dashboard screenshots

Five pages in both themes at 1440 px, drawn by the real renderer from the test fixtures, not from anyone's memory: the Overview healthy, learning (a new install) and with the server down, a session and a memory. The README shows `overview-healthy-*.png`.

To take them again from the repository root, with Chrome or Chromium on the `PATH`:

```sh
BACKSIGHT_SCREENSHOTS="$PWD/docs/assets/dashboard" go test ./internal/dashboard -run TestScreenshots -count=1
```
