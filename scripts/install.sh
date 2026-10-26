#!/bin/sh
# backsight bootstrap (SPEC §11.1): downloads the release for this machine,
# checks its minisign signature against the key below and its checksum,
# installs it with the signed checksums.txt (the binary re-verifies itself
# against it), points the shim <data>/bin/current at it and the command on
# PATH at the shim, and says what to run next. POSIX sh; needs curl (or a local
# release directory), tar, and minisign or openssl (3.0+, for Ed25519).
#
#   curl -fsSL <url>/install.sh | sh
#
# BACKSIGHT_RELEASE_DIR  a local directory holding a release (checksums.txt,
#                        its .minisig and the archives): no network
# BACKSIGHT_RELEASE_URL  where the release files are, as
#                        https://github.com/Barben360/backsight/releases/download/<tag>
#                        (default: the newest release, pre-releases included,
#                        found through the GitHub API without a token)
# BACKSIGHT_HOME         as for backsight itself: one directory for everything
# BACKSIGHT_INSTALL_PUBKEY  tests only: another trusted key
# BACKSIGHT_INSTALL_GITHUB  tests only: one https host standing in for
#                        api.github.com and github.com
#
# The two test-only overrides stay in the published script so that its
# tests run the very script users get. They are read from the environment
# of whoever runs it, never from the network or a file, and whoever sets
# that environment already controls every tool this script calls (PATH:
# minisign, openssl, tar). Either one set, the script says so on stderr,
# naming the key or host, so nobody installs unknowingly from another
# source (security review L3).
#
# macOS ships LibreSSL, which cannot check these signatures: install
# minisign there (brew install minisign).
set -eu

# Everything runs from main, called on the last line: a download cut short
# runs nothing.
main() {
# The release key (decision 0027). Changed only by a key rotation.
PUBKEY=${BACKSIGHT_INSTALL_PUBKEY:-RWTezqkiNo2ey8msd7vpSC0+D6oghZ1/VwgXU6EqYRjlZzyd87+/03eG}
REPO=Barben360/backsight
API=${BACKSIGHT_INSTALL_GITHUB:-https://api.github.com}
DOWNLOADS=${BACKSIGHT_INSTALL_GITHUB:-https://github.com}/$REPO/releases/download
URL=${BACKSIGHT_RELEASE_URL:-}

die() { echo "backsight install: $*" >&2; exit 1; }
warn() { echo "backsight install: WARNING: $*" >&2; }

[ -z "${BACKSIGHT_INSTALL_PUBKEY:-}" ] ||
	warn "BACKSIGHT_INSTALL_PUBKEY is set: the release is checked against $PUBKEY, not backsight's release key (a test setting: unset it unless you are testing)"
[ -z "${BACKSIGHT_INSTALL_GITHUB:-}" ] ||
	warn "BACKSIGHT_INSTALL_GITHUB is set: the release comes from $BACKSIGHT_INSTALL_GITHUB, not GitHub (a test setting: unset it unless you are testing)"

case $(uname -s) in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported system $(uname -s): backsight installs on Linux and macOS only for now (platforms: https://github.com/$REPO/blob/main/docs/compatibility.md)" ;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported machine $(uname -m)" ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'rm -rf "$tmp"; exit 130' INT TERM

fetch() { # fetch <name>: into $tmp
	if [ -n "${BACKSIGHT_RELEASE_DIR:-}" ]; then
		cp "$BACKSIGHT_RELEASE_DIR/$1" "$tmp/$1" 2>/dev/null || die "$1 is not in $BACKSIGHT_RELEASE_DIR"
	else
		curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/$1" "$URL/$1" || die "cannot download $URL/$1"
	fi
}

# verify <file>: <file>.minisig must be a prehashed minisign signature by
# PUBKEY, over the file and over its trusted comment.
verify() {
	f=$1
	sigfile=$f.minisig
	if command -v minisign >/dev/null 2>&1; then
		# -H: prehashed signatures only, as the openssl path accepts
		minisign -VqH -P "$PUBKEY" -m "$f" -x "$sigfile" || die "$(basename "$f"): bad signature"
		return
	fi
	command -v openssl >/dev/null 2>&1 || die "neither minisign nor openssl is installed: install one to check the release signature"
	case $(openssl version 2>/dev/null) in
	"OpenSSL "[3-9]*) ;;
	*) die "$(openssl version 2>/dev/null | cut -d' ' -f1-2) cannot check Ed25519 signatures: install minisign (or OpenSSL 3)" ;;
	esac
	d=$tmp/verify
	mkdir -p "$d"
	printf '%s' "$PUBKEY" | base64 -d > "$d/pub.bin" 2>/dev/null || die "bad public key"
	sed -n 2p "$sigfile" | base64 -d > "$d/sig.bin" 2>/dev/null || die "$(basename "$sigfile"): unreadable"
	[ "$(head -c 2 "$d/sig.bin")" = ED ] || die "$(basename "$sigfile"): not a prehashed minisign signature"
	[ "$(head -c 10 "$d/sig.bin" | tail -c 8 | od -An -tx1)" = "$(head -c 10 "$d/pub.bin" | tail -c 8 | od -An -tx1)" ] ||
		die "$(basename "$sigfile"): signed by another key"
	{ printf '\060\052\060\005\006\003\053\145\160\003\041\000'; tail -c 32 "$d/pub.bin"; } > "$d/pub.der"
	openssl pkey -pubin -inform DER -in "$d/pub.der" -out "$d/pub.pem" 2>/dev/null || die "openssl cannot read an Ed25519 key (needs OpenSSL 3)"
	tail -c 64 "$d/sig.bin" > "$d/s.bin"
	openssl dgst -blake2b512 -binary "$f" > "$d/hash.bin"
	openssl pkeyutl -verify -pubin -inkey "$d/pub.pem" -rawin -in "$d/hash.bin" -sigfile "$d/s.bin" >/dev/null 2>&1 ||
		die "$(basename "$f"): bad signature"
	comment=$(sed -n 3p "$sigfile")
	case $comment in "trusted comment: "*) ;; *) die "$(basename "$sigfile"): no trusted comment" ;; esac
	{ cat "$d/s.bin"; printf '%s' "${comment#trusted comment: }"; } > "$d/global.bin"
	sed -n 4p "$sigfile" | base64 -d > "$d/g.bin" 2>/dev/null || die "$(basename "$sigfile"): unreadable"
	openssl pkeyutl -verify -pubin -inkey "$d/pub.pem" -rawin -in "$d/global.bin" -sigfile "$d/g.bin" >/dev/null 2>&1 ||
		die "$(basename "$sigfile"): bad trusted comment signature"
}

# newest: the tag of the newest release, pre-releases included. GitHub's
# /releases/latest skips pre-releases; the release list, newest first, does
# not. No jq: the first "tag_name" key of the list is read with awk.
newest() {
	api=$API/repos/$REPO/releases?per_page=1
	how="set BACKSIGHT_RELEASE_URL=$DOWNLOADS/<tag> (tags: https://github.com/$REPO/releases), or BACKSIGHT_RELEASE_DIR to a downloaded release"
	curl -fsSL --proto '=https' --tlsv1.2 -H 'Accept: application/vnd.github+json' -o "$tmp/releases.json" "$api" ||
		die "cannot list the releases at $api: $how"
	tag=$(awk '
		{ s = s $0 "\n" }
		END {
			k = "\"tag_name\""
			while ((i = index(s, k)) > 0) {
				s = substr(s, i + length(k))
				sub(/^[ \t\r\n]*/, "", s)
				if (substr(s, 1, 1) != ":") continue
				s = substr(s, 2)
				if (!sub(/^[ \t\r\n]*"/, "", s)) exit
				print substr(s, 1, index(s, "\"") - 1)
				exit
			}
		}' "$tmp/releases.json")
	case $tag in
	"") die "no release found at $api: $how" ;;
	.* | *[!A-Za-z0-9._+-]*) die "unexpected release tag at $api: $how" ;;
	esac
	echo "newest release: $tag"
	URL=$DOWNLOADS/$tag
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

[ -n "${BACKSIGHT_RELEASE_DIR:-}" ] || [ -n "$URL" ] || newest
fetch checksums.txt
fetch checksums.txt.minisig
verify "$tmp/checksums.txt"
# The trusted comment is signed: it must name the file it was made for.
case $(sed -n 3p "$tmp/checksums.txt.minisig") in
*"file:checksums.txt"*) ;;
*) die "checksums.txt.minisig was made for another file" ;;
esac

archive=$(awk -v s="_${os}_${arch}.tar.gz" '{ n = $2; if (index(n, "backsight_") == 1 && substr(n, length(n) - length(s) + 1) == s) print n }' "$tmp/checksums.txt" | head -n 1)
[ -n "$archive" ] || die "the release has no archive for $os/$arch"
case $archive in */* | .*) die "unexpected archive name $archive" ;; esac
want=$(awk -v n="$archive" '$2 == n { print $1 }' "$tmp/checksums.txt")
fetch "$archive"
[ "$(sha256 "$tmp/$archive")" = "$want" ] || die "$archive: checksum mismatch"

version=${archive#backsight_}
version=${version%_"${os}"_"${arch}".tar.gz}
# The version names the install directory, <data>/bin/<version>.
case $version in "" | .* | *[!A-Za-z0-9._+-]*) die "unexpected release version in $archive" ;; esac
mkdir "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x" backsight || die "$archive holds no backsight binary"

if [ -n "${BACKSIGHT_HOME:-}" ]; then
	data=$BACKSIGHT_HOME/data
	bindir=$BACKSIGHT_HOME/bin
elif [ "$os" = darwin ]; then
	data=$HOME/Library/Application\ Support/backsight
	bindir=$HOME/.local/bin
else
	data=${XDG_DATA_HOME:-$HOME/.local/share}/backsight
	bindir=$HOME/.local/bin
fi
dest=$data/bin/$version
mkdir -p "$dest" "$bindir"
[ -f "$tmp/x/backsight" ] && [ ! -L "$tmp/x/backsight" ] || die "$archive: backsight is not a regular file"
install -m 0755 "$tmp/x/backsight" "$dest/backsight.tmp" || die "cannot write $dest"
mv -f "$dest/backsight.tmp" "$dest/backsight" || die "cannot write $dest"
# The binary checks itself against the signed checksums on its first run.
for f in checksums.txt checksums.txt.minisig; do
	{ cp "$tmp/$f" "$dest/$f.tmp" && mv -f "$dest/$f.tmp" "$dest/$f"; } || die "cannot write $dest"
done
# The shim every generated file runs, then the command on PATH: a link to
# the shim, never to a versioned binary (decision 0067 block review), so
# it follows update apply and never dangles when old binaries are pruned.
# Each link is swapped whole (ln -s then mv: no moment without one).
shim=$data/bin/current
link() { # link <target> <name>
	rm -f "$2.tmp"
	ln -s "$1" "$2.tmp" || die "cannot write $(dirname "$2")"
	mv -f "$2.tmp" "$2" || die "cannot write $(dirname "$2")"
}
link "$dest/backsight" "$shim"
link "$shim" "$bindir/backsight"

echo "backsight $version installed: $bindir/backsight"
case :$PATH: in
*:"$bindir":*) ;;
*) echo "add it to your PATH first: export PATH=\"$bindir:\$PATH\" (in your shell's profile to keep it)" ;;
esac
echo "next: backsight setup"
echo "a coding agent installing for you: backsight guide agent-guide"
}

main "$@"
