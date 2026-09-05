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
#
# The agent is put in a session of its own.  Some agent CLIs return while
# helpers they started remain alive; reaping that session before this wrapper
# returns prevents those helpers from contending for shared agent state (most
# notably OAuth token refreshes) with the next turn.
set -u
[ -n "${FACTORYD_PROGRESS:-}" ] || { echo "turn-wrapper: FACTORYD_PROGRESS is not set; not running under a factoryd supervisor" >&2; exit 3; }
[ $# -ge 1 ] || { echo "turn-wrapper: no agent command given" >&2; exit 3; }
# Keep this list in step with doctor.turnWrapperTools: these are resolved from
# the role's constructed PATH, never factoryd's inherited environment.
for tool in setsid sh stat grep mktemp cat rm sleep; do
  command -v "$tool" >/dev/null 2>&1 || { echo "turn-wrapper: $tool is required from the role PATH to derive progress and reap agent helpers" >&2; exit 3; }
done
# Nanosecond precision: a turn that touches the marker within the same
# second as the baseline is real progress, and a whole-second compare would
# turn it into a failure -- the worse mistake. GNU stat's %.9Y; a stat that
# cannot give it is refused rather than degraded to seconds.
mtime() { stat -c %.9Y "$1" 2>/dev/null; }
if [ -e "$FACTORYD_PROGRESS" ] && ! mtime "$FACTORYD_PROGRESS" | grep -q '\.[0-9]\{9\}$'; then
  echo "turn-wrapper: stat cannot report nanosecond mtimes here; refusing to derive an exit code from whole seconds" >&2; exit 3
fi
before=$(mtime "$FACTORYD_PROGRESS" || echo none)
# Run setsid in the foreground: an asynchronously started setsid can itself be
# a process-group leader and has to fork, losing the PID we need to reap. The
# small session leader writes its own PID before it execs the actual agent, so
# the wrapper retains the agent-only process-group id after the leader exits.
# The explicit input dup preserves a piped prompt for agent commands such as
# `claude -p` and `codex exec -`.
agent_pid_file=$(mktemp "${TMPDIR:-/tmp}/factoryd-agent-session.XXXXXX") || { echo "turn-wrapper: cannot make the agent-session record" >&2; exit 3; }
trap 'rm -f "$agent_pid_file"' 0
setsid sh -c 'printf "%s\\n" "$$" > "$1"; shift; exec "$@"' sh "$agent_pid_file" "$@" <&0
rc=$?
agent_pid=$(cat "$agent_pid_file" 2>/dev/null || true)
rm -f "$agent_pid_file"
trap - 0

# The agent leader is gone now, but helper processes can still hold the new
# session's process group. It contains the agent only: this wrapper and the
# supervisor remain in their original group, so signalling it cannot kill the
# process deciding this turn's result. The runner's cgroup reaper remains the
# final containment backstop for a helper that deliberately starts another
# session.
case "$agent_pid" in
  ''|*[!0-9]*) agent_pid="" ;;
esac
group_gone() { ! kill -0 "-$agent_pid" 2>/dev/null; }
wait_for_group_gone() {
  tries=0
  while ! group_gone && [ "$tries" -lt 40 ]; do
    sleep 0.05
    tries=$((tries + 1))
  done
  group_gone
}
if [ -n "$agent_pid" ] && kill -0 "-$agent_pid" 2>/dev/null; then
  echo "turn-wrapper: agent exited with helper processes still running; reaping them before the next turn so they cannot contend for shared agent credentials" >&2
  kill -TERM "-$agent_pid" 2>/dev/null || true
  if ! wait_for_group_gone; then
    echo "turn-wrapper: agent helpers ignored TERM; sending KILL before returning" >&2
    kill -KILL "-$agent_pid" 2>/dev/null || true
    if ! wait_for_group_gone; then
      # Do not report a clean turn while the process group may still be
      # running. The runner's cgroup containment must now kill and verify it;
      # if that cannot happen, this failure stops the turn rather than opening
      # a window for the next agent to contend with the helper.
      echo "turn-wrapper: could not prove agent helpers gone after KILL; refusing a clean turn so factoryd containment can take over" >&2
      exit 3
    fi
  fi
fi
after=$(mtime "$FACTORYD_PROGRESS" || echo none)
if [ "$rc" -ne 0 ]; then
  exit "$rc"
fi
if [ "$before" = "$after" ]; then
  echo "turn-wrapper: the agent exited 0 but $FACTORYD_PROGRESS did not move; the turn achieved nothing it could show, exiting 1" >&2
  exit 1
fi
exit 0
