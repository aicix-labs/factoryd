#!/bin/sh
# Run a model-driven producer turn (#38). The intent protocol lives HERE,
# once, not in every brief: a brief describes work; this composes the
# handshake around it and runs the agent through turn-wrapper.sh, which
# derives the exit code from the progress marker (#27).
#
# What the model must be told, and what nothing else tells it:
#   - declare intent by writing .producer-branch and .producer-commit-msg
#     in the workdir; nothing is submitted without both; a gate runs
#     before any push; no git, no network, no push from the turn itself.
#   - touch $FACTORYD_PROGRESS when it advanced; consume its trigger.
#   - on a verdict trigger, re-declare the family named by
#     FACTORYD_CHANGE_BRANCH, VERBATIM: a fix is a new immutable branch
#     that supersedes the old draft only under the same family (#29).
#     Omitting this is the easy mistake, and it opens an unrelated draft.
#
# Usage in factory.json:
#   "command": ["/usr/local/lib/factoryd/producer-turn-agent.sh",
#               "codex", "exec", "--full-auto", "-"]
# The agent command reads the composed prompt on stdin. Set
# PRODUCER_PROMPT_FILE to also write the prompt there (for the record).
set -u
[ -n "${FACTORYD_WORKDIR:-}" ] || { echo "producer-turn-agent: FACTORYD_WORKDIR is not set; not running under a factoryd supervisor" >&2; exit 3; }
[ -n "${FACTORYD_PROGRESS:-}" ] || { echo "producer-turn-agent: FACTORYD_PROGRESS is not set" >&2; exit 3; }
[ $# -ge 1 ] || { echo "producer-turn-agent: no agent command given" >&2; exit 3; }
here=$(dirname "$0")
wrapper="$here/turn-wrapper.sh"
[ -x "$wrapper" ] || { echo "producer-turn-agent: $wrapper is not executable; the exit code cannot be derived" >&2; exit 3; }

compose() {
  cat <<PROTO
You are the PRODUCER in a factoryd factory. One turn: read the work below, edit
the tree in $FACTORYD_WORKDIR, run the tests you can run, declare intent, exit.

THE PROTOCOL (this is how work leaves your hands; nothing else does):
1. Declare intent by writing TWO files in the workdir, both required:
     $FACTORYD_WORKDIR/.producer-branch      one line: the branch FAMILY name
     $FACTORYD_WORKDIR/.producer-commit-msg  the commit message, first line the title
   Nothing is submitted without both. A gate (build, vet, tests) runs before any
   push; a red gate comes back to you as a question, not as a merge.
2. You have no git remote, no network, and no credential. Do NOT try to push,
   open a change, or fetch. factoryd does that after you exit, from these files.
3. When you have advanced, touch $FACTORYD_PROGRESS. A turn that changes nothing
   should touch nothing and say so.
4. If you decide there is nothing to do, write neither file.
PROTO
  case "${FACTORYD_TRIGGERS:-}" in
    *verdict*)
      if [ -n "${FACTORYD_CHANGE_BRANCH:-}" ]; then
        cat <<VERDICT

THIS TURN IS A VERDICT on a change you submitted earlier. Re-declare the SAME
FAMILY, exactly as written here, in .producer-branch:
    $FACTORYD_CHANGE_BRANCH
A fix is a new immutable branch that supersedes the old draft only under this
family name. Any other name opens an unrelated draft beside the one under review.
The verdicts, as JSON: $FACTORYD_VERDICTS
VERDICT
      else
        cat <<VERDICTS

THIS TURN CARRIES SEVERAL VERDICTS. Each names its family in declared_branch;
re-declare the one you are acting on, exactly as written:
$FACTORYD_VERDICTS
VERDICTS
      fi
      ;;
  esac
  brief="${FACTORYD_INBOX:-}/brief.md"
  if [ -r "$brief" ]; then
    printf '\nTHE WORK (from the operator):\n\n'
    cat "$brief"
  fi
  printf '\nWhen done: the two files (or neither), touch %s, exit 0.\n' "$FACTORYD_PROGRESS"
}

prompt=$(compose) || { echo "producer-turn-agent: could not compose the prompt" >&2; exit 3; }
case "$prompt" in
  *".producer-branch"*".producer-commit-msg"*) ;;
  *) echo "producer-turn-agent: the composed prompt does not carry the intent protocol; refusing to run the agent without it" >&2; exit 3;;
esac
if [ -n "${PRODUCER_PROMPT_FILE:-}" ]; then
  printf '%s\n' "$prompt" > "$PRODUCER_PROMPT_FILE" || { echo "producer-turn-agent: cannot write $PRODUCER_PROMPT_FILE" >&2; exit 3; }
fi
printf '%s\n' "$prompt" | "$wrapper" "$@"
rc=$?
# The trigger is consumed by the turn that acted on it. The agent cannot be
# relied on to do this; the wrapper does, after the agent, whatever it did.
IFS=:; for p in ${FACTORYD_TRIGGER_PATHS:-}; do [ -n "$p" ] && rm -f "$p"; done
exit $rc
