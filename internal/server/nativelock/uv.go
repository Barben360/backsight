package nativelock

// The uv release that installs Python and the lock (Task 1 C6, ruling R24):
// its Linux x86_64 archive and that archive's sha256, checked before
// anything is extracted.
const (
	UVVersion = "0.13.6"
	UVAsset   = "uv-x86_64-unknown-linux-gnu.tar.gz"
	UVSHA256  = "f29c3e94ffabd7e6f3ba521dba169a9b7ec12100bf3257087f0f24f6123f3645"

	// Windows (M7 block 2, E34 W6): the zip and the release's .sha256 asset.
	UVWindowsAsset  = "uv-x86_64-pc-windows-msvc.zip"
	UVWindowsSHA256 = "a2a8820d5eea32f08ef467ef3b7f66883a9b076d5737a2fc2fbb5f458d1bb50f"
)
