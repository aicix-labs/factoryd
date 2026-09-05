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
  printf '%s\n' "${FACTORYD_VERDICTS_TSV:-}" | while IFS="$tab" read -r vpath vid vkind vbranch vfam; do
    [ -n "$vid" ] && printf '  - change %s: %s%s\n' "$vid" "${vkind:-unknown}" "${vfam:+ (family $vfam)}"
  done
}

# selected_cr prints the ONE changes-requested verdict this turn acts on --
# the first, in trigger order -- as "path<TAB>id<TAB>family". A turn has one
# .producer-branch / .producer-commit-msg pair, so it can submit one
# successor; the other changes-requested verdicts keep their triggers and
# get their own turns (#50 review).
selected_cr() {
  printf '%s\n' "${FACTORYD_VERDICTS_TSV:-}" | while IFS="$tab" read -r vpath vid vkind vbranch vfam; do
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
  keep=$(printf '%s\n' "${FACTORYD_VERDICTS_TSV:-}" | while IFS="$tab" read -r vpath vid vkind vbranch vfam; do
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
# The progress marker is snapshotted before the agent runs (cp -p keeps
# the nanosecond mtime). If the selected verdict ends the turn unresolved,
# the marker is put back exactly as it was: a model that touched progress
# and declared nothing, or the wrong family, made no progress ON THE
# VERDICT, and reporting its touch as progress would reset the
# supervisor's guards and re-run the kept trigger forever (#50 review).
# With no progress and a non-zero exit the supervisor backs off and
# halts at fail_abort, the verdict still in the outbox.
snap=$(mktemp "${TMPDIR:-/tmp}/factoryd-progress.XXXXXX") || exit 3
had_progress=0
if [ -e "$FACTORYD_PROGRESS" ]; then cp -p "$FACTORYD_PROGRESS" "$snap" || exit 3; had_progress=1; fi
# Observable work is progress even without a declaration: a large fix
# legitimately spans turns, and a model that edits the tree and waits to
# declare until the fix is complete is advancing (#50 review). The tree is
# fingerprinted before and after by CONTENT, type and mode -- every file's
# bytes, every symlink's target, every directory, .git and the control
# files aside -- never by timestamp: a touch is not work, a heartbeat file
# refreshed each turn is not work (#50 review, round 8). A changed tree
# keeps the model's progress and the verdict for the next turn, up to a
# bound: even content changes can be meaningless, so after
# PRODUCER_VERDICT_ATTEMPTS (default 6) partial turns on the same verdict
# without a declaration, further partial turns are no progress, and the
# supervisor halts with the verdict kept.
if command -v sha256sum >/dev/null 2>&1; then hasher=sha256sum; else hasher=cksum; fi
tree_fingerprint() {
  (cd "$FACTORYD_WORKDIR" && {
     find . -path ./.git -prune -o ! -name '.producer-branch*' ! -name '.producer-commit-msg*' \( -type f -o -type l -o -type d \) -printf '%y %m %p -> %l\n'
     find . -path ./.git -prune -o -type f ! -name '.producer-branch*' ! -name '.producer-commit-msg*' -print0 | LC_ALL=C sort -z | xargs -0 -r "$hasher"
   } 2>/dev/null | LC_ALL=C sort | "$hasher")
}
tree_before=$(tree_fingerprint)
attempt_bound=${PRODUCER_VERDICT_ATTEMPTS:-6}
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
# A resolved verdict clears its attempt count.
if [ -n "$sel" ] && [ "$declared" -eq 1 ]; then
  rm -f "$FACTORYD_INBOX/.verdict-attempts-$(printf '%s' "$sel" | cut -f2)"
fi
# The declaration must be FOR the selected verdict: .producer-branch must
# equal its family exactly (#50 review). A complete declaration under
# another name would consume this verdict and open an unrelated draft --
# the stale-family failure #29 addressed. On a mismatch the wrong
# declaration is moved aside (never the source work), the verdict is
# kept, and the turn exits non-zero so the supervisor's after-turn step
# does not run: nothing is submitted, and the retry is told the verdict.
if [ -n "$sel" ] && [ "$declared" -eq 1 ]; then
  sel_fam=$(printf '%s' "$sel" | cut -f3)
  got_fam=$(sed -n '1{s/[[:space:]]*$//;p;}' "$FACTORYD_WORKDIR/.producer-branch")
  if [ "$got_fam" != "$sel_fam" ]; then
    stamp=$(date +%s)
    mv "$FACTORYD_WORKDIR/.producer-branch" "$FACTORYD_WORKDIR/.producer-branch.wrong-family-$stamp"
    mv "$FACTORYD_WORKDIR/.producer-commit-msg" "$FACTORYD_WORKDIR/.producer-commit-msg.wrong-family-$stamp"
    echo "producer-turn-agent: the turn declared family '$got_fam' but the selected verdict is on '$sel_fam'; declaration moved aside, verdict kept, turn failed" >&2
    declared=0
    rc=4
  fi
fi
if [ -n "$sel" ] && [ "$declared" -eq 0 ]; then
  keep="$keep
$sel_path"
  sel_id=$(printf '%s' "$sel" | cut -f2)
  attempts_file="$FACTORYD_INBOX/.verdict-attempts-$sel_id"
  attempts=$(cat "$attempts_file" 2>/dev/null || echo 0)
  if [ "$rc" -eq 0 ] && [ "$(tree_fingerprint)" != "$tree_before" ] && [ "$attempts" -lt "$attempt_bound" ]; then
    # Partial work: the tree's content changed. The model's progress
    # stands, the verdict waits for the next turn, and the turn is clean.
    attempts=$((attempts + 1))
    printf '%s\n' "$attempts" > "$attempts_file"
    echo "producer-turn-agent: the selected changes-requested verdict is kept for the next turn: the tree changed but no complete intent was declared yet (partial turn $attempts of $attempt_bound)" >&2
  else
    if [ "$rc" -eq 0 ] && [ "$attempts" -ge "$attempt_bound" ]; then
      echo "producer-turn-agent: $attempts partial turns on verdict $sel_id without a declaration; the bound is reached, this turn is no progress" >&2
    fi
    echo "producer-turn-agent: the selected changes-requested verdict is kept: the turn $( [ "$rc" -eq 0 ] && echo 'changed nothing and declared no intent' || echo "exited $rc" )" >&2
    # No progress on the verdict: the marker goes back to its baseline, and
    # the turn is a failure, so the supervisor's guards count it.
    if [ "$had_progress" -eq 1 ]; then touch -r "$snap" "$FACTORYD_PROGRESS"; else rm -f "$FACTORYD_PROGRESS"; fi
    [ "$rc" -eq 0 ] && rc=4
  fi
fi
rm -f "$snap"
IFS=:; for p in ${FACTORYD_TRIGGER_PATHS:-}; do
  case "$p" in *-retry) continue;; esac
  [ -n "$p" ] || continue
  case "$keep" in *"$p"*) continue;; esac
  rm -f "$p"
done
exit $rc
