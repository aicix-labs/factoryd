#!/bin/sh
# Run a model-driven producer turn (#38). The intent protocol lives HERE,
# once, not in every brief: a brief describes work; this composes the
# handshake around it and runs the agent through turn-wrapper.sh, which
# derives the exit code from the progress marker (#27).
#
# What the model must be told, and what nothing else tells it:
#   - declare intent by writing .producer-branch and .producer-commit-msg
#     in the workdir; nothing is submitted without both; a gate runs
#     before any push; no SCM remote, no provider credential, no push or
#     fetch from the turn itself (a hosted model still reaches its API).
#   - touch $FACTORYD_PROGRESS when it advanced; consume its trigger.
#   - on a verdict trigger, act BY KIND: changes-requested re-declares
#     the family named by FACTORYD_CHANGE_BRANCH, VERBATIM -- a fix is a
#     new immutable branch that supersedes the old draft only under the
#     same family (#29); merged and operator-gated declare NOTHING -- a
#     re-declaration there resubmits a change that has left the
#     producer's hands, the loop #40 was about.
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

# verdict_kinds lists "change_id kind declared_branch" per verdict from
# FACTORYD_VERDICTS. No JSON tool is assumed; the mapping is one object per
# verdict with string values, which this extracts field by field.
verdict_kinds() {
  printf '%s\n' "${FACTORYD_VERDICTS:-[]}" | tr '{' '\n' | while IFS= read -r obj; do
    id=$(printf '%s' "$obj" | sed -n 's/.*"change_id":"\([^"]*\)".*/\1/p')
    kind=$(printf '%s' "$obj" | sed -n 's/.*"kind":"\([^"]*\)".*/\1/p')
    fam=$(printf '%s' "$obj" | sed -n 's/.*"declared_branch":"\([^"]*\)".*/\1/p')
    [ -n "$id" ] && printf '  - change %s: %s%s\n' "$id" "${kind:-unknown}" "${fam:+ (declared_branch $fam)}"
  done
}

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
2. You have no git remote and no provider credential. Do NOT try to push, fetch,
   or open a change. factoryd does that after you exit, from these files.
3. When you have advanced, touch $FACTORYD_PROGRESS. A turn that changes nothing
   should touch nothing and say so.
4. If you decide there is nothing to do, write neither file.
PROTO
  case "${FACTORYD_TRIGGERS:-}" in
    *verdict*)
      # Verdicts are acted on BY KIND. The wrapper reads them so the model
      # is told, per change, whether to declare anything at all: a merged
      # or operator-gated change has left the producer's hands, and a
      # re-declaration there resubmits it (#40); only changes-requested
      # re-declares, and then the exact family (#29).
      kinds=$(verdict_kinds)
      cat <<VERDICT

THIS TURN IS A VERDICT on work you submitted earlier. Act on each verdict BY ITS
KIND, and on nothing else:
  - merged: your change landed. Declare NOTHING for it (write neither file).
    Do not resubmit; a re-declaration would open a new draft of finished work.
  - operator-gated: a human must act on it. Declare NOTHING for it; wait.
  - changes-requested: fix it, then re-declare the SAME FAMILY, exactly as
    written in declared_branch, in .producer-branch. A fix is a new immutable
    branch that supersedes the old draft only under this family name; any
    other name opens an unrelated draft beside the one under review.
The verdicts this turn, by change and kind:
$kinds
VERDICT
      case "$kinds" in
        *changes-requested*)
          if [ -n "${FACTORYD_CHANGE_BRANCH:-}" ]; then
            printf 'The family to re-declare for the changes-requested verdict, verbatim:\n    %s\n' "$FACTORYD_CHANGE_BRANCH"
          else
            printf 'Re-declare the declared_branch of the changes-requested verdict, verbatim.\n'
          fi;;
        *) printf 'No verdict this turn asks for a declaration. Write neither file unless the brief below asks for new work.\n';;
      esac
      printf 'The verdicts, as JSON: %s\n' "${FACTORYD_VERDICTS:-[]}"
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
# The supervisor's retry marker is NOT the turn's to remove: the supervisor
# owns it, removes it after the retry ran, and keeps it on purpose when the
# retry halts, as the record of what was being retried.
IFS=:; for p in ${FACTORYD_TRIGGER_PATHS:-}; do
  case "$p" in *-retry) continue;; esac
  [ -n "$p" ] && rm -f "$p"
done
exit $rc
