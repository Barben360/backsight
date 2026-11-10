#!/usr/bin/env bash
# A recording hook for every Codex event (probe-hooks.json wires it): it
# saves the payload Codex sends to /out/probe/<time>-<event>.json, then
# answers what /tmp/probe/<event>.{stdout,stderr,rc} hold (nothing and exit
# 0 when absent), so one run can try an output Codex must accept: an
# additionalContext, a systemMessage, a decision block, an exit 2.
event=${1:?event}
mkdir -p /out/probe
id=$(date +%s%N)-$$
cat > "/out/probe/$id-$event.json"
env | sort > "/out/probe/$id-$event.env"
c=/tmp/probe/$event
rc=$(cat "$c.rc" 2>/dev/null || echo 0)
[ -f "$c.stdout" ] && cat "$c.stdout"
[ -f "$c.stderr" ] && cat "$c.stderr" >&2
# .once: the answer is for the next run only.
[ -f "$c.once" ] && rm -f "$c.stdout" "$c.stderr" "$c.rc" "$c.once"
exit "$rc"
