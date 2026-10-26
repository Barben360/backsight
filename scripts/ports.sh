# shellcheck shell=bash
# The API ports of the scratch stacks (issue #214), sourced by the scripts
# that start one. All sit below Linux's ephemeral range (32768-60999, where
# an outgoing connection can briefly hold a port) and away from this
# machine's live installs (8888/9999 and 18888/19999). Each script has its
# own base; BACKSIGHT_CI_SLOT (1-3 on CI, set by the workflows; 0 when
# unset off CI) adds slot x 10, so no two scripts or slots share a port.
# On CI (CI set) an unset slot is refused: on a machine running several
# CI jobs, slot 0 would share another job's project and ports. The UI
# takes the API port + 1111.
#
#   base   script            API (slot 0-3)  UI (slot 0-3)
#   28300  sandbox.sh        28300-28330     29411-29441
#   28350  restore-proof.sh  28350-28380     29461-29491
#   28400  fresh-install.sh  28400-28430     29511-29541
#   28450  agent-install.sh  28450-28480     29561-29591
#   28500  reattribute-proof.sh 28500-28530  29611-29641

# slot_port base [port]: the port given, else base + slot x 10; refuses a
# slot outside 0-3, no slot on CI, and an API or UI port of the live
# installs.
slot_port() {
  local slot=${BACKSIGHT_CI_SLOT:-0} port p
  if [ -z "${BACKSIGHT_CI_SLOT:-}" ] && [ -n "${CI:-}" ]; then
    echo "BACKSIGHT_CI_SLOT is not set on this CI runner: set it (1, 2 or 3) in the workflow's env" >&2
    return 2
  fi
  case "$slot" in
    [0-3]) ;;
    *) echo "BACKSIGHT_CI_SLOT=$slot: expected 1, 2 or 3 (unset off CI)" >&2; return 2 ;;
  esac
  port=${2:-$(($1 + slot * 10))}
  case "$port" in
    '' | *[!0-9]*) echo "port '$port' is not a number" >&2; return 2 ;;
  esac
  for p in "$port" "$((port + 1111))"; do
    case "$p" in
      8888 | 9999 | 18888 | 19999) echo "port $p belongs to a live install on this machine" >&2; return 2 ;;
    esac
  done
  echo "$port"
}
