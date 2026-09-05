#!/bin/sh
# One producer turn. Sandboxed: no network, no git. Reads its trigger from
# FACTORYD_TRIGGERS; a verdict trigger means the previous change landed.
set -u
log() { echo "producer-turn: $*" >&2; }
log "triggers=${FACTORYD_TRIGGERS:-} workdir=$(pwd) uid=$(id -u)"
# Evidence the sandbox holds: this must fail.
if (exec 3<>/dev/tcp/127.0.0.1/22) 2>/dev/null; then log "NETWORK REACHABLE -- sandbox not holding"; exit 9; fi
log "network unreachable (as required)"
case "${FACTORYD_TRIGGERS:-}" in
  *verdict*) log "a verified verdict arrived: ${FACTORYD_VERDICTS_TSV:-}"; touch "$FACTORYD_PROGRESS" || exit 1
             IFS=:; for p in $FACTORYD_TRIGGER_PATHS; do rm -f "$p"; done; exit 0;;
esac
mkdir -p auth
printf 'package auth\n\n// session handling, acceptance run %s\n' "$(date +%s)" > auth/session.go
printf 'accept/session\n' > .producer-branch
printf 'auth: session handling for the acceptance run\n' > .producer-commit-msg
touch "$FACTORYD_PROGRESS" || { log "cannot record progress"; exit 1; }
IFS=:; for p in $FACTORYD_TRIGGER_PATHS; do rm -f "$p"; done
log "declared intent on accept/session; consumed the brief"
exit 0
