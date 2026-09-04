#!/bin/sh
# Run an agent CLI as a factoryd turn and DERIVE the exit code (#27).
#
# `claude -p` and `codex exec` exit 0 whatever the model concludes; a model
# cannot set an exit code by narrating "the turn closed non-zero". The
# supervisor reads the exit code, so a turn that decided it had nothing to
# do, and correctly touched nothing, would read as a clean turn that
# achieved nothing -- forever. The observable effect is the progress marker
# (SPEC §6.3): a turn that advanced touched $FACTORYD_PROGRESS. This wrapper
# exits non-zero when it did not, whatever the agent's own exit code, and
# passes a non-zero agent exit through unchanged.
#
# Usage in factory.json:
#   "command": ["/usr/local/lib/factoryd/turn-wrapper.sh", "claude", "-p", "..."]
set -u
[ -n "${FACTORYD_PROGRESS:-}" ] || { echo "turn-wrapper: FACTORYD_PROGRESS is not set; not running under a factoryd supervisor" >&2; exit 3; }
[ $# -ge 1 ] || { echo "turn-wrapper: no agent command given" >&2; exit 3; }
before=$(stat -c %Y "$FACTORYD_PROGRESS" 2>/dev/null || echo none)
"$@"
rc=$?
after=$(stat -c %Y "$FACTORYD_PROGRESS" 2>/dev/null || echo none)
if [ "$rc" -ne 0 ]; then
  exit "$rc"
fi
if [ "$before" = "$after" ]; then
  echo "turn-wrapper: the agent exited 0 but $FACTORYD_PROGRESS did not move; the turn achieved nothing it could show, exiting 1" >&2
  exit 1
fi
exit 0
