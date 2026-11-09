#!/usr/bin/env bash
# Records scripts/testshard/durations.txt, the times make test-shard
# balances its slices by, from the go test -json each test-race slice of a
# CI run uploads (artifacts test-race-times-<slice>): the given run, or the
# latest successful run of ci on main. Every slice must be there, and
# testshard -record refuses any failure: the file is replaced only then.
# The artifacts land in .cache/test-durations/<run id> (gitignored), kept
# and read again for the same run.
#
#   scripts/test-durations.sh [<run id>]
#
# OUT and CACHE move the file written and the downloads (the tests).
set -euo pipefail

cd "$(dirname "$0")/.."
out=${OUT:-scripts/testshard/durations.txt}
slices=4 # ci.yml test-race's matrix (workflows_test.go)

run=${1:-}
if [ -z "$run" ]; then
  run=$(gh run list --workflow ci --branch main --event push --status success --limit 1 --json databaseId --jq '.[0].databaseId')
  [ -n "$run" ] || { echo "test-durations.sh: no successful ci run on main" >&2; exit 1; }
fi
case "$run" in
'' | *[!0-9]*) echo "test-durations.sh: run id $run is not a number" >&2; exit 1 ;;
esac

dir=${CACHE:-.cache/test-durations}/$run
if [ ! -d "$dir" ]; then
  part="$dir.part.$$" # a failed download leaves it, under the ignored .cache
  mkdir -p "$part"
  gh run download "$run" --pattern 'test-race-times-*' --dir "$part"
  mv "$part" "$dir"
fi
files=()
for s in $(seq 0 $((slices - 1))); do
  f="$dir/test-race-times-$s/test-race-$s.json"
  [ -s "$f" ] || { echo "test-durations.sh: run $run has no times for slice $s ($f)" >&2; exit 1; }
  files+=("$f")
done
cat "${files[@]}" | go run ./scripts/testshard -record > "$out.new"
mv "$out.new" "$out"
echo "test-durations.sh: $out from run $run ($(grep -cv "^#" "$out") tests)"
