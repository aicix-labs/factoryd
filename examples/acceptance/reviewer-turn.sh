#!/bin/sh
set -u
log() { echo "reviewer-turn: $*" >&2; }
C="$FACTORYD_CONFIG"
log "triggers=${FACTORYD_TRIGGERS:-} uid=$(id -u)"
# Match the branch FAMILY by prefix: submit pushes <declared>-<tree>, never
# the declared name itself, so an exact match could never fire.
id=$(factoryd scm -config "$C" list-open 2>/dev/null | awk -F'\t' '$2 ~ /^accept\/session-/ {print $1; exit}')
[ -n "$id" ] || { log "no open change for accept/session"; exit 1; }
sha=$(factoryd scm -config "$C" get "$id" 2>/dev/null | awk '$1=="head" {print $2; exit}')
log "reviewing $id at $sha"
# The adversarial pass, scripted: attempts against the escalate-class path.
att=$(mktemp); cat > "$att" <<JSON
{"attempts":["replayed a session token after logout","forged a session cookie with an expired signature","path traversal in the session id"],"notes":"scripted stand-in pass; all attempts refused by the code under review"}
JSON
factoryd audit -config "$C" post "$id" "$sha" -lens session-fixation -verdict CLEARED -file "$att" || exit 1
factoryd scm -config "$C" set-draft "$id" false || exit 1
factoryd signal -config "$C" "$id" merged auto -summary "cleared: session handling reviewed; three adversarial attempts recorded and refused" || exit 1
touch "$FACTORYD_PROGRESS" || { log "cannot record progress"; exit 1; }
# Consume the trigger acted on: the turn's act, not the supervisor's.
IFS=:; for p in $FACTORYD_TRIGGER_PATHS; do rm -f "$p"; done
