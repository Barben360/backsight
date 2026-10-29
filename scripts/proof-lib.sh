#!/usr/bin/env bash
# Shared helpers of the proof scripts (fresh-install, the VM bench, the CI
# chains). Sourced, never executed; the caller defines `fail`.
#
# pl_secret <var> <cmd...>   runs cmd with xtrace off, stores its stdout in
#                            var, puts xtrace back as the caller had it and
#                            returns cmd's status.
# pl_untraced <cmd...>       the same, stdout passed through.
# pl_nonempty <var>          succeeds when the variable NAMED holds something.
# pl_differ <var1> <var2>    succeeds when the two variables NAMED differ.
#                            Both run with xtrace off: no value in a traced argv.
# pl_hash <file...>          sha256sum of each; a file that cannot be read
#                            ends in `fail "cannot hash <file>"`.
#
# A command line is traced before it runs: never put a secret in the argv of
# the call itself. Pass the command a function that reads the secret inside.

pl_untraced() {
  local _pl_traced=0 _pl_rc=0
  case $- in *x*) _pl_traced=1 ;; esac
  { set +x; } 2>/dev/null
  "$@" || _pl_rc=$? # the wrapped command runs with set -e inert, because of this ||
  if [ "$_pl_traced" = 1 ]; then set -x; fi
  return "$_pl_rc"
}

pl_secret() {
  local _pl_var=$1 _pl_traced=0 _pl_rc=0 _pl_out=
  shift
  case $- in *x*) _pl_traced=1 ;; esac
  { set +x; } 2>/dev/null
  _pl_out=$("$@") || _pl_rc=$? # the wrapped command runs with set -e inert, because of this ||
  printf -v "$_pl_var" %s "$_pl_out"
  if [ "$_pl_traced" = 1 ]; then set -x; fi
  return "$_pl_rc"
}

pl_nonempty() { pl_untraced _pl_nonempty "$1"; }
_pl_nonempty() { [ -n "${!1:-}" ]; }
pl_differ() { pl_untraced _pl_differ "$1" "$2"; }
_pl_differ() { [ "${!1:-}" != "${!2:-}" ]; }

pl_hash() {
  local _pl_f
  for _pl_f in "$@"; do
    [ -f "$_pl_f" ] && [ -r "$_pl_f" ] || { fail "cannot hash $_pl_f"; return 1; }
  done
  sha256sum -- "$@" || { fail "cannot hash $*"; return 1; }
}
