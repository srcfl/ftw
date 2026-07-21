#!/usr/bin/env bash
# All-in-one supervisor for the FTW Home Assistant add-on.
#
# Two bundled processes share one container:
#   - CORE (primary): the Go app — state, safety, dispatch, web UI/API on :8080.
#   - OPTIMIZER (optional): the Python/CVXPY planner, reached by core over the
#     Unix socket in /run/ftw-optimizer.
#
# Supervision model (why this is not a symmetric "wait on either"):
#   - CORE is the add-on. When it exits we exit with ITS code, so Home
#     Assistant Supervisor sees success vs. failure correctly and restarts the
#     add-on on a crash.
#   - The OPTIMIZER is optional: core runs FTW_OPTIMIZER_TRANSPORT=auto and
#     degrades (bundled Python worker -> in-Go DP planner) when the socket is
#     down. So the optimizer dying must NOT take core down. We restart it with
#     capped backoff to recover from a transient crash; after repeated failures
#     we give up and leave core running degraded, rather than crash-looping the
#     whole add-on.
#   - tini (PID 1) reaps zombies; SIGTERM/SIGINT from Supervisor tears both
#     down and exits 0 (a clean stop, not a failure).
set -uo pipefail

mkdir -p /run/ftw-optimizer /data/drivers

# --- optional optimizer, kept alive in the background --------------------
supervise_optimizer() {
  local child="" delay=1 tries=0 start rc
  # Own trap: when the parent stops us, take the current optimizer child with
  # us. child stays "" until the first spawn, so we never kill process group 0.
  trap 'if [ -n "$child" ]; then kill "$child" 2>/dev/null; fi; exit 0' TERM INT
  while :; do
    start=$SECONDS
    /opt/venv/bin/ftw-optimizer &
    child=$!
    wait "$child"
    rc=$?
    # Ran healthily for a while -> reset the crash-loop backoff.
    if [ $((SECONDS - start)) -ge 60 ]; then
      tries=0
      delay=1
    fi
    tries=$((tries + 1))
    if [ "$tries" -ge 10 ]; then
      echo "run.sh: optimizer keeps exiting (last rc=$rc); leaving core on the DP planner" >&2
      return 0
    fi
    echo "run.sh: optimizer exited (rc=$rc); restart ${tries}/10 in ${delay}s" >&2
    sleep "$delay"
    [ "$delay" -lt 30 ] && delay=$((delay * 2))
  done
}
supervise_optimizer &
OPT_SUP=$!

# --- core: the add-on's primary process ----------------------------------
/app/ftw -config /data/config.yaml -web /app/web -drivers /app/drivers -user-drivers /data/drivers &
CORE_PID=$!

# Supervisor stop -> tear both down and exit cleanly (0, not a failure).
shutdown() {
  trap - TERM INT
  kill "$CORE_PID" 2>/dev/null || true
  kill "$OPT_SUP"  2>/dev/null || true   # its own trap stops the optimizer
  exit 0
}
trap shutdown TERM INT

# Block on CORE only. Optimizer restarts happen inside its supervisor and never
# reach here, so core is free to degrade instead of being torn down with it.
wait "$CORE_PID"
rc=$?

# Core is gone -> the add-on is done. Stop the optimizer supervisor and
# propagate core's exit code so Supervisor restarts the add-on on failure.
kill "$OPT_SUP" 2>/dev/null || true
exit "$rc"
