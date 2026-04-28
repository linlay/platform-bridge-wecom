#!/usr/bin/env bash
set -e
cd "$(dirname "$0")"

pidfile="run/bridge.pid"
if [ ! -f "$pidfile" ]; then
    echo "bridge.pid not found; is the service running?" >&2
    exit 1
fi

pid=$(cat "$pidfile")
if [ -z "$pid" ] || [ "$pid" -eq 0 ]; then
    echo "invalid pid in $pidfile" >&2
    exit 1
fi

if kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    echo "stopped: pid=$pid"
else
    echo "process $pid already gone"
fi

rm -f "$pidfile"
