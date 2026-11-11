#!/usr/bin/env bash
# The backsight shim in the Codex container: runs the real binary (@REAL@,
# and @BSHOME@ filled in by prepare.sh) and records each run under /out/tap:
# a hook's stdin payload, stdout, exit code and environment, the MCP
# server's whole JSON-RPC traffic in both directions with its environment,
# and the arguments of any other run (a hook started without its own).
# When /tmp/bs-hints exists, the server's tools/list answer gains
# destructiveHint and openWorldHint false on memory_store and memory_credit
# (what the Codex server is to declare), to see Codex's approvals with them.
real=@REAL@
d=/out/tap
env0=$(env | sort) # as Codex gave it
# Codex hands an MCP server only a short list of environment variables
# (HOME, PATH, ...): the throwaway backsight home is named here, as the
# default ~/.backsight would be found without it.
export BACKSIGHT_HOME=@BSHOME@
id=$(date +%s%N)-$$
case "${1:-}" in
  hook)
    ev=${*: -1} # the event: the last word (hook <event>, or hook --harness codex <event>)
    in=$(cat)
    printf '%s' "$in" > "$d/$id-hook-$ev.in.json"
    printf '%s\n' "$env0" > "$d/$id-hook-$ev.env"
    out=$(printf '%s' "$in" | "$real" "$@" 2> "$d/$id-hook-$ev.err")
    rc=$?
    printf '%s' "$out" > "$d/$id-hook-$ev.out"
    echo "$rc" > "$d/$id-hook-$ev.rc"
    printf '%s' "$out"
    exit "$rc"
    ;;
  mcp)
    printf '%s\n' "$env0" > "$d/$id-mcp.env"
    hints='.'
    if [ -e /tmp/bs-hints ]; then
      hints='if .result.tools then .result.tools |= map(if .name == "memory_store" or .name == "memory_credit" then .annotations = ((.annotations // {}) + {destructiveHint: false, openWorldHint: false}) else . end) else . end'
    fi
    tee -a "$d/$id-mcp.in.jsonl" | "$real" "$@" 2> "$d/$id-mcp.err" | jq -c --unbuffered "$hints" | tee -a "$d/$id-mcp.out.jsonl"
    ;;
  *)
    printf '%s\n' "$#" "$@" > "$d/$id-other.args"
    exec "$real" "$@"
    ;;
esac
