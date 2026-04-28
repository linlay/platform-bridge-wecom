#!/usr/bin/env bash
set -e
cd "$(dirname "$0")"

if [ ! -f .env ]; then
    echo "missing .env file" >&2
    exit 1
fi

mkdir -p run

set -a
source .env
set +a

nohup ./platform-bridge-wecom > run/bridge.log 2> run/bridge.stderr.log &
echo $! > run/bridge.pid
echo "started: pid=$(cat run/bridge.pid)"
