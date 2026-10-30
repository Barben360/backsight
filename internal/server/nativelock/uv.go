package nativelock

// The uv release that installs Python and the lock (Task 1 C6, ruling R24):
// its Linux x86_64 archive and that archive's sha256, checked before
// anything is extracted.
const (
	UVVersion = "0.13.1"
	UVAsset   = "uv-x86_64-unknown-linux-gnu.tar.gz"
	UVSHA256  = "eadd9d27c134ab5ac1ffd4473b97a741407f3ea238288f03121fc4deddf90220"
)
