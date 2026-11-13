#!/usr/bin/env bash
# native-lock.sh [--check]: the native runtime's hash-locked Python set
# (decision 0078 requirement 4), E29 install.sh's method. The pinned
# Hindsight image (render.go's pinnedHindsight) is read once with a
# throwaway `docker run --rm` (nothing else is touched): its Python version
# and its site-packages become the constraints (torch dropped: the native
# runtime runs onnx only). `uv pip compile --generate-hashes` then resolves
# hindsight-api-slim[local-onnx,embedded-db]==<the image's tag> and
# pg0-embedded==0.15.2 for Linux x86_64 under those constraints into
# internal/server/nativelock/requirements-linux-amd64.txt, which backsight
# embeds and installs with `uv pip install --require-hashes --no-deps`.
#
# It fails when the image ships another hindsight-api-slim than its tag,
# when its Python is not 3.11, and when a resolved package is neither the
# image's own nor an override (a deliberate exception is named in
# NATIVE_LOCK_EXTRA, space-separated package names, and recorded in the
# header).
#
# internal/server/nativelock/overrides.txt (NATIVE_LOCK_OVERRIDES names
# another file, tests) lists the deliberate departures from the image,
# "name==version: reason" per line: a security fix the image lacks, or a
# package such a fix requires. Each replaces the image's version in the
# constraints (a plain constraint, not uv's --override: every package's
# declared requirements still hold), must be in the resolved lock at
# exactly that version, and is recorded in the header as "# override:
# name==version (image: version|none)".
# An override named in FORCED (or NATIVE_LOCK_FORCED, tests) is passed to
# uv as --override instead: a dependent's declared cap on it no longer
# holds. Use it only for a security fix a dependent caps without needing
# to, with the reason beside the name. An override the image ships at
# the same or a newer version is refused (PEP 440 order, pep440_cmp): drop it.
#
# --check regenerates into a temporary file and fails when any
# name==version line or the header differs from the committed lock; the
# hashes are not compared (PyPI may add wheels to a release).
#
# --platform windows (M7 block 2, decision 0080) makes the Windows lock,
# requirements-windows-amd64.txt: the same roots plus msvc-runtime, resolved
# for x86_64-pc-windows-msvc with every name==version of the Linux lock as a
# constraint, so a package both locks hold has one version. It needs the
# Linux lock, uv and PyPI, not Docker. Its header repeats the Linux lock's
# "# override:" lines. --check-shared compares the two
# committed locks offline: a package in both with two versions must be in
# WINDOWS_EXCEPTIONS, a package only in the Windows lock must be in
# WINDOWS_ONLY.
#
# Needs Docker (the image, pulled if absent) and uv on PATH, and the
# network (PyPI). NATIVE_LOCK names another lock file (tests);
# NATIVE_LOCK_LINUX the Linux lock the Windows one is tied to.
set -euo pipefail
# Byte order for every sort and comm, whatever the caller's locale: the
# header is compared across machines, and uutils' comm (Ubuntu 26.04)
# checks byte order where its sort follows the locale.
export LC_ALL=C
root=$(cd "$(dirname "$0")/.." && pwd)
linux_lock=${NATIVE_LOCK_LINUX:-$root/internal/server/nativelock/requirements-linux-amd64.txt}
pg0=0.15.2
msvc_runtime=14.44.35112
# The packages only the Windows lock holds (markers in the wheels' metadata),
# and the shared packages allowed two versions ("name: reason", one per line).
WINDOWS_ONLY="msvc-runtime winloop colorama tzdata pywin32 pywin32-ctypes"
WINDOWS_EXCEPTIONS=""
# Packages whose Linux version is forced on the Windows resolve (uv --override,
# "name: reason", one per line): a dependent's Windows-only cap would else
# pull them to another version.
WINDOWS_OVERRIDES="onnxruntime: markitdown 0.1.5b1 caps it at 1.20.1 on win32 only; Linux runs the locked version with it"
# Overrides forced past a dependent's declared cap (uv --override, "name:
# reason", one per line). Each must also be in overrides.txt.
FORCED=${NATIVE_LOCK_FORCED-"filelock: litellm 1.104.0 declares filelock<4.0, but its proxy CLI is its only import, with FileLock(path, timeout=, mode=); huggingface-hub (FileLock, SoftFileLock, WeakFileLock, Timeout) declares filelock>=3.10 with no cap; both APIs are unchanged in 4.x; litellm 1.109.1 declares filelock<5"}
check=0 shared=0 target=linux
while [ $# -gt 0 ]; do
  case $1 in
  --check) check=1 ;;
  --check-shared) shared=1 ;;
  --platform)
    [ $# -ge 2 ] || { echo "usage: scripts/native-lock.sh [--platform linux|windows] [--check|--check-shared]" >&2; exit 2; }
    target=$2
    shift
    ;;
  *)
    echo "usage: scripts/native-lock.sh [--platform linux|windows] [--check|--check-shared]" >&2
    exit 2
    ;;
  esac
  shift
done
case $target in
linux)
  platform=x86_64-manylinux_2_28
  lock=${NATIVE_LOCK:-$linux_lock}
  ;;
windows)
  platform=x86_64-pc-windows-msvc
  lock=${NATIVE_LOCK:-$root/internal/server/nativelock/requirements-windows-amd64.txt}
  ;;
*)
  echo "native-lock: --platform is linux or windows, not $target" >&2
  exit 2
  ;;
esac
[ "$shared" = 0 ] || [ "$target" = windows ] || { echo "native-lock: --check-shared needs --platform windows" >&2; exit 2; }

die() {
  echo "native-lock: $*" >&2
  exit 1
}

# Each FORCED line is "name: reason": a line without one would be skipped.
while IFS= read -r l; do
  [ -z "$l" ] || printf '%s\n' "$l" | grep -Eq '^[A-Za-z0-9._-]+: [^ ]' || die "FORCED line \"$l\" is not \"name: reason\""
done <<EOF_FORCED
$FORCED
EOF_FORCED

image=$(sed -n 's/^const pinnedHindsight = "\(.*\)"$/\1/p' "$root/internal/server/render.go")
[ -n "$image" ] || die "no pinnedHindsight in internal/server/render.go"
tag=${image%%@*}
tag=${tag##*:}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# norm: name==version lines with PEP 503 names (lowercase, runs of -_. as -).
norm() { awk -F'==' 'NF == 2 { n = tolower($1); gsub(/[-_.]+/, "-", n); print n "==" $2 }'; }
# reqs: a lock's name==version lines (hashes and markers dropped).
reqs() { grep -E '^[A-Za-z0-9]' "$1" | sed -E 's/[ ;\\].*//' | norm | sort; }

# pep440_cmp a b: PEP 440 order (-1, 0, 1).
# shellcheck source=scripts/pep440.sh
. "$root/scripts/pep440.sh"

# shared_check: the Windows lock $1 against the Linux lock $2.
shared_check() {
  reqs "$2" > "$work/lin.txt"
  reqs "$1" > "$work/win.txt"
  bad=$(awk -F'==' -v only=" $WINDOWS_ONLY " -v exc="$(printf '%s\n' "$WINDOWS_EXCEPTIONS" | sed 's/:.*//' | tr '\n' ' ')" '
    NR == FNR { lin[$1] = $2; next }
    ($1 in lin) { if (lin[$1] != $2 && index(" " exc " ", " " $1 " ") == 0) print $1 ": Linux " lin[$1] ", Windows " $2 " (not in WINDOWS_EXCEPTIONS)"; next }
    index(only, " " $1 " ") == 0 { print $1 "==" $2 ": only in the Windows lock, not in WINDOWS_ONLY" }
  ' "$work/lin.txt" "$work/win.txt")
  [ -z "$bad" ] || die "the Windows lock strays from the Linux lock:
$(echo "$bad" | sed 's/^/  /')"
}

if [ "$target" = windows ]; then
  [ -f "$linux_lock" ] || die "no Linux lock at $linux_lock: run scripts/native-lock.sh"
  if [ "$shared" = 1 ]; then
    [ -f "$lock" ] || die "no lock at $lock: run scripts/native-lock.sh --platform windows"
    shared_check "$lock" "$linux_lock"
    echo "native-lock: $lock agrees with the Linux lock ($(reqs "$lock" | wc -l) packages, windows-only: $WINDOWS_ONLY)"
    exit 0
  fi
  image=$(sed -n 's/^# image: //p' "$linux_lock")
  pyline=$(sed -n 's/^# python: //p' "$linux_lock")
  [ -n "$image" ] && [ -n "$pyline" ] || die "the Linux lock $linux_lock has no image or python header"
  py=${pyline%% *}
  hash12=$(sha256sum "$linux_lock" | cut -c1-12)
  reqs "$linux_lock" > "$work/constraints.txt"
  tag=${image%%@*}
  tag=${tag##*:}
  : > "$work/overrides.txt"
  # FORCED's packages are forced here too: the same cap would pull them back.
  for o in $( (printf '%s\n' "$WINDOWS_OVERRIDES"; printf '%s\n' "$FORCED") | sed -n 's/:.*//p'); do
    grep "^$o==" "$work/constraints.txt" >> "$work/overrides.txt" || die "WINDOWS_OVERRIDES or FORCED names $o, which the Linux lock lacks"
  done
  printf '%s\n' "hindsight-api-slim[local-onnx,embedded-db]==$tag" "pg0-embedded==$pg0" "msvc-runtime==$msvc_runtime" > "$work/in.txt"
  env -u UV_INDEX_URL -u UV_EXTRA_INDEX_URL -u UV_DEFAULT_INDEX -u UV_INDEX -u UV_FIND_LINKS \
    -u PIP_INDEX_URL -u PIP_EXTRA_INDEX_URL UV_NO_CONFIG=1 \
    uv pip compile --quiet --no-header --no-annotate --generate-hashes \
    --python-version "$py" --python-platform "$platform" \
    -c "$work/constraints.txt" --override "$work/overrides.txt" -o "$work/body.txt" "$work/in.txt"
  {
    echo "# Generated by scripts/native-lock.sh --platform windows: do not edit."
    echo "# image: $image"
    echo "# python: $pyline"
    echo "# platform: $platform"
    echo "# linux-lock: $hash12"
    grep '^# override: ' "$linux_lock" || true
    cat "$work/body.txt"
  } > "$work/lock.txt"
  shared_check "$work/lock.txt" "$linux_lock"
  if [ "$check" = 1 ]; then
    [ -f "$lock" ] || die "no lock at $lock: run scripts/native-lock.sh --platform windows"
    if ! diff <(grep '^# ' "$lock"; reqs "$lock") <(grep '^# ' "$work/lock.txt"; reqs "$work/lock.txt") > "$work/diff"; then
      sed 's/^/  /' "$work/diff" >&2
      die "$lock is out of date (< committed, > regenerated): run scripts/native-lock.sh --platform windows"
    fi
    echo "native-lock: $lock is current ($(reqs "$lock" | wc -l) packages)"
    exit 0
  fi
  cp "$work/lock.txt" "$lock"
  echo "native-lock: wrote $lock ($(reqs "$lock" | wc -l) packages, tied to Linux lock $hash12)"
  exit 0
fi

docker run --rm --entrypoint sh "$image" -c 'python3 --version; ls /app/api/.venv/lib/python3.11/site-packages' > "$work/image.txt"
py=$(sed -n '1s/^Python \(3\.11\.[0-9][0-9]*\)$/\1/p' "$work/image.txt")
[ -n "$py" ] || die "the image's Python is not 3.11: $(head -1 "$work/image.txt")"

sed -n 's/\.dist-info$//p' "$work/image.txt" | sed -E 's/^(.*)-([0-9][^-]*)$/\1==\2/' | norm |
  grep -v '^torch==' | sort > "$work/constraints.txt"
shipped=$(sed -n 's/^hindsight-api-slim==//p' "$work/constraints.txt")
[ "$shipped" = "$tag" ] || die "the image $image ships hindsight-api-slim ${shipped:-none}, its tag says $tag"

printf '%s\n' "hindsight-api-slim[local-onnx,embedded-db]==$tag" "pg0-embedded==$pg0" > "$work/in.txt"

# The overrides: name==version (PEP 503 names), checked line by line, then
# "# override:" header lines; the constraints take their versions.
overrides_file=${NATIVE_LOCK_OVERRIDES:-$root/internal/server/nativelock/overrides.txt}
[ -f "$overrides_file" ] || die "no overrides file at $overrides_file"
bad=$(awk '/^[[:space:]]*(#|$)/ { next }
  !/^[A-Za-z0-9][A-Za-z0-9._-]*==[^[:space:]:]+: [^[:space:]]/ { print "line " NR ": " $0 }' "$overrides_file")
[ -z "$bad" ] || die "$overrides_file: want name==version: reason, not
$(echo "$bad" | sed 's/^/  /')"
{ grep -Ev '^[[:space:]]*(#|$)' "$overrides_file" || true; } | sed 's/: .*//' | norm | sort > "$work/overrides.txt"
twice=$(sed 's/==.*//' "$work/overrides.txt" | uniq -d | tr '\n' ' ')
[ -z "$twice" ] || die "$overrides_file names ${twice% } twice"
: > "$work/override-header.txt"
while IFS='=' read -r name _ ver; do
  iv=$(sed -n "s/^$name==//p" "$work/constraints.txt")
  if [ -n "$iv" ]; then
    c=$(pep440_cmp "$ver" "$iv") || die "cannot compare $name's override $ver with the image's $iv (not a normalised PEP 440 version): check it by hand"
    [ "$c" = 1 ] || die "the image ships $name $iv, not older than its override $ver: drop it from $overrides_file"
  fi
  echo "# override: $name==$ver (image: ${iv:-none})" >> "$work/override-header.txt"
done < "$work/overrides.txt"
# The header's order is the C locale's (byte order): --check compares it,
# and another locale orders fastmcp and fastmcp-slim the other way.
LC_ALL=C sort -o "$work/override-header.txt" "$work/override-header.txt"
awk -F'==' -v ov="$work/overrides.txt" 'BEGIN { while ((getline l < ov) > 0) { split(l, a, "=="); o[a[1]] } }
  !($1 in o)' "$work/constraints.txt" | cat - "$work/overrides.txt" | sort > "$work/pins.txt"
mv "$work/pins.txt" "$work/constraints.txt"
: > "$work/forced.txt"
for f in $(printf '%s\n' "$FORCED" | sed -n 's/:.*//p'); do
  grep "^$f==" "$work/overrides.txt" >> "$work/forced.txt" || die "FORCED names $f, which $overrides_file does not override"
done

# The user's own uv and pip settings (an index, a config file) stay out:
# the lock is PyPI's. It is resolved for the image's exact Python, the one
# the runtime installs: a package may require a patch release (soupsieve
# 4.x needs 3.11.5).
env -u UV_INDEX_URL -u UV_EXTRA_INDEX_URL -u UV_DEFAULT_INDEX -u UV_INDEX -u UV_FIND_LINKS \
  -u PIP_INDEX_URL -u PIP_EXTRA_INDEX_URL UV_NO_CONFIG=1 \
  uv pip compile --quiet --no-header --no-annotate --generate-hashes \
  --python-version "$py" --python-platform "$platform" \
  -c "$work/constraints.txt" --override "$work/forced.txt" -o "$work/body.txt" "$work/in.txt"

unheld=$(comm -23 "$work/overrides.txt" <(reqs "$work/body.txt") | tr '\n' ' ')
[ -z "$unheld" ] || die "the resolved lock does not hold the override(s) ${unheld% }: nothing requires them, or another version won; fix $overrides_file"
extra=${NATIVE_LOCK_EXTRA:-}
missing=$(comm -23 <(reqs "$work/body.txt") "$work/constraints.txt" |
  awk -v extra=" $extra " -F'==' 'index(extra, " " $1 " ") == 0')
[ -z "$missing" ] || die "resolved packages that are not the image's or an override: $(echo "$missing" | tr '\n' ' ')(NATIVE_LOCK_EXTRA names a deliberate one)"

out=$work/lock.txt
{
  echo "# Generated by scripts/native-lock.sh: do not edit."
  echo "# image: $image"
  echo "# python: $py (image: $py)"
  echo "# platform: $platform"
  cat "$work/override-header.txt"
  [ -z "$extra" ] || echo "# not in the image: $extra"
  cat "$work/body.txt"
} > "$out"

if [ "$check" = 1 ]; then
  [ -f "$lock" ] || die "no lock at $lock: run scripts/native-lock.sh"
  if ! diff <(grep '^# ' "$lock"; reqs "$lock") <(grep '^# ' "$out"; reqs "$out") > "$work/diff"; then
    sed 's/^/  /' "$work/diff" >&2
    die "$lock is out of date (< committed, > regenerated): run scripts/native-lock.sh"
  fi
  echo "native-lock: $lock is current ($(reqs "$lock" | wc -l) packages, hindsight-api-slim $tag, Python $py)"
  exit 0
fi
cp "$out" "$lock"
echo "native-lock: wrote $lock ($(reqs "$lock" | wc -l) packages, hindsight-api-slim $tag, Python $py)"
