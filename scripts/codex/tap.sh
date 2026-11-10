#!/usr/bin/env bash
# The backsight shim in the Codex container: runs the real binary (@REAL@,
# and @BSHOME@ filled in by prepare.sh) and records each run under /out/tap: a hook's
# stdin payload, stdout, exit code and environment, and the MCP server's
# whole JSON-RPC traffic in both directions with its environment.
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
    in=$(cat)
    printf '%s' "$in" > "$d/$id-hook-$2.in.json"
    printf '%s\n' "$env0" > "$d/$id-hook-$2.env"
    out=$(printf '%s' "$in" | "$real" "$@" 2> "$d/$id-hook-$2.err")
    rc=$?
    printf '%s' "$out" > "$d/$id-hook-$2.out"
    echo "$rc" > "$d/$id-hook-$2.rc"
    printf '%s' "$out"
    exit "$rc"
    ;;
  mcp)
    printf '%s\n' "$env0" > "$d/$id-mcp.env"
    tee -a "$d/$id-mcp.in.jsonl" | "$real" "$@" 2> "$d/$id-mcp.err" | tee -a "$d/$id-mcp.out.jsonl"
    ;;
  *) exec "$real" "$@" ;;
esac
