#!/usr/bin/env bash
# Frees a hosted CI runner's disk for the Docker and native stack jobs
# (ci.yml sandbox, fresh-install, m6-chain, native-chain), only when it is
# short: the images, the model cache and the data need room, and setup's
# doctor wants 6 GB free. The runner's preinstalled toolchains go when
# less than 20 GiB is free; a runner of 2026-11 has 86 GB free, and
# removing them took 1.5 to 3 minutes of the job. A df that gives no
# number fails the step rather than pass as enough room.
#
#   scripts/ci-free-disk.sh
set -euo pipefail

min_kib=$((20 * 1024 * 1024))

df -h /
avail=$(df --output=avail -k / | tail -n 1 | tr -d ' ')
case "$avail" in
'' | *[!0-9]*) echo "ci-free-disk.sh: df gave no free space for / (got '$avail')" >&2; exit 1 ;;
esac
if [ "$avail" -ge "$min_kib" ]; then
  echo "ci-free-disk.sh: $((avail / 1024 / 1024)) GiB free, nothing removed"
  exit 0
fi
sudo rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc /opt/hostedtoolcache/CodeQL
df -h /
