#!/usr/bin/env bash
# All-in-one supervisor for the FTW Home Assistant add-on.
#
# Runs the two bundled processes — the Python/CVXPY optimizer and the Go core —
# in one container. They share the Unix socket at /run/ftw-optimizer. If either
# process exits, we tear the other down and exit non-zero so Home Assistant
# Supervisor restarts the whole add-on cleanly (rather than limping on with
# half the stack). tini (PID 1) reaps everything.
set -uo pipefail

mkdir -p /run/ftw-optimizer /data/drivers

# 1) Python/CVXPY optimizer daemon — creates the socket that core dials.
/opt/venv/bin/ftw-optimizer &
OPT_PID=$!

# 2) Go core — transport=auto uses the socket above, else the in-Go DP
#    planner. Serves the web UI / HTTP API on :8080.
/app/ftw -config /data/config.yaml -web /app/web -drivers /app/drivers -user-drivers /data/drivers &
CORE_PID=$!

shutdown() { kill "$OPT_PID" "$CORE_PID" 2>/dev/null || true; }
trap shutdown INT TERM

# Return as soon as EITHER child exits.
wait -n "$OPT_PID" "$CORE_PID"
shutdown
wait 2>/dev/null || true
