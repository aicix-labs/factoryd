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
#     the family, VERBATIM, from the runner's FACTORYD_VERDICTS_TSV -- a
#     fix is a new immutable branch that supersedes the old draft only
#     under the same family (#29); merged and operator-gated declare
#     NOTHING -- a re-declaration there resubmits a change that has left
#     the producer's hands, the loop #40 was about. ONE changes-requested
#     verdict per turn: a turn has one declaration; the others keep their
#     triggers and get their own turns.
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

# The verdicts come pre-rendered by the runner in FACTORYD_VERDICTS_TSV: one
# line per verdict, tab-separated path, change_id, kind, branch,
# declared_branch. Git refuses control characters in refnames, so no field
# can hold a tab or a newline, and "{" in a family is just a character. No
# JSON is parsed here (#50 review).
tab=$(printf '\t')

# verdict_lines prints every verdict as "  - change ID: KIND (family F)".
verdict_lines() {
  printf '%s' "${FACTORYD_VERDICTS_TSV:-}" | while IFS="$tab" read -r vpath vid vkind vbranch vfam; do
    [ -n "$vid" ] && printf '  - change %s: %s%s\n' "$vid" "${vkind:-unknown}" "${vfam:+ (family $vfam)}"
  done
}

# selected_cr prints the ONE changes-requested verdict this turn acts on --
# the first, in trigger order -- as "path<TAB>id<TAB>family". A turn has one
# .producer-branch / .producer-commit-msg pair, so it can submit one
# successor; the other changes-requested verdicts keep their triggers and
# get their own turns (#50 review).
selected_cr() {
  printf '%s' "${FACTORYD_VERDICTS_TSV:-}" | while IFS="$tab" read -r vpath vid vkind vbranch vfam; do
    if [ "$vkind" = "changes-requested" ]; then printf '%s\t%s\t%s\n' "$vpath" "$vid" "$vfam"; break; fi
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
      [ -n "${FACTORYD_VERDICTS_TSV:-}" ] || { echo "producer-turn-agent: a verdict trigger but no FACTORYD_VERDICTS_TSV; this factoryd is too old for this wrapper" >&2; return 3; }
      cat <<VERDICT

THIS TURN IS A VERDICT on work you submitted earlier. Act BY KIND, on nothing
else:
  - merged: your change landed. Declare NOTHING for it (write neither file).
    Do not resubmit; a re-declaration would open a new draft of finished work.
  - operator-gated: a human must act on it. Declare NOTHING for it; wait.
  - changes-requested: fix it, then re-declare the SAME FAMILY, exactly as
    written below, in .producer-branch. A fix is a new immutable branch that
    supersedes the old draft only under this family name; any other name opens
    an unrelated draft beside the one under review.
The verdicts this turn, by change and kind:
$(verdict_lines)
VERDICT
      sel=$(selected_cr)
      if [ -n "$sel" ]; then
        sel_id=$(printf '%s' "$sel" | cut -f2)
        sel_fam=$(printf '%s' "$sel" | cut -f3)
        printf 'THIS TURN ACTS ON ONE changes-requested verdict: change %s. Fix it and re-declare\nits family, verbatim:\n    %s\nAny other changes-requested verdict listed above gets its own turn later; do not\nwork on it now.\n' "$sel_id" "$sel_fam"
      else
        printf 'No verdict this turn asks for a declaration. Write neither file unless the brief\nbelow asks for new work.\n'
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
# Which changes-requested triggers stay for later turns: every one but the
# selected. (merged / operator-gated triggers carry no work; consumed.)
keep=""
sel_path=""
sel=$(selected_cr)
if [ -n "$sel" ]; then
  sel_path=$(printf '%s' "$sel" | cut -f1)
  keep=$(printf '%s' "${FACTORYD_VERDICTS_TSV:-}" | while IFS="$tab" read -r vpath vid vkind vbranch vfam; do
    [ "$vkind" = "changes-requested" ] && [ "$vpath" != "$sel_path" ] && printf '%s\n' "$vpath"
  done)
fi
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
# A changes-requested trigger not acted on this turn is kept: it is the
# next turn's work, and deleting it would lose that verdict for good. The
# SELECTED one is consumed only when the turn both succeeded and left a
# complete declaration (#50 review): a model that exited non-zero, or
# exited clean having declared nothing, has not acted on the verdict, and
# a retry without the trigger would have no verdict, family or work to
# retry. Then the supervisor's guards see an unconsumed trigger and say so.
declared=0
if [ "$rc" -eq 0 ] \
   && [ -f "$FACTORYD_WORKDIR/.producer-branch" ] && [ ! -L "$FACTORYD_WORKDIR/.producer-branch" ] && [ -s "$FACTORYD_WORKDIR/.producer-branch" ] \
   && [ -f "$FACTORYD_WORKDIR/.producer-commit-msg" ] && [ ! -L "$FACTORYD_WORKDIR/.producer-commit-msg" ] && [ -s "$FACTORYD_WORKDIR/.producer-commit-msg" ]; then
  declared=1
fi
if [ -n "$sel" ] && [ "$declared" -eq 0 ]; then
  keep="$keep
$sel_path"
  echo "producer-turn-agent: the selected changes-requested verdict is kept: the turn $( [ "$rc" -eq 0 ] && echo 'declared no complete intent' || echo "exited $rc" )" >&2
fi
IFS=:; for p in ${FACTORYD_TRIGGER_PATHS:-}; do
  case "$p" in *-retry) continue;; esac
  [ -n "$p" ] || continue
  case "$keep" in *"$p"*) continue;; esac
  rm -f "$p"
done
exit $rc
