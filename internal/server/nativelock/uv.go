package nativelock

// The uv release that installs Python and the lock (Task 1 C6, ruling R24):
// its Linux x86_64 archive and that archive's sha256, checked before
// anything is extracted.
const (
	UVVersion = "0.13.1"
	UVAsset   = "uv-x86_64-unknown-linux-gnu.tar.gz"
	UVSHA256  = "eadd9d27c134ab5ac1ffd4473b97a741407f3ea238288f03121fc4deddf90220"

	// Windows (M7 block 2, E34 W6): the zip and the release's .sha256 asset.
	UVWindowsAsset  = "uv-x86_64-pc-windows-msvc.zip"
	UVWindowsSHA256 = "3b2661a463a5baea7f5076ea2b25ab71d77f36a6c0d809e5350651792f80a672"
)
