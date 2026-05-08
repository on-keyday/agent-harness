#!/bin/bash
# Emits ~1.5 MiB to fill the 1-MiB ring (so replay = 1 MiB), THEN ticks every
# 200ms forever. Used to verify reattach output flow when ring is saturated.
set -e
trap 'exit 0' SIGTERM SIGHUP SIGINT
yes "burst line padding to overflow the ring buffer for replay-saturation tests" | head -c 1500000
echo
n=0
while true; do
    printf 'tick %d\n' "$n"
    n=$((n+1))
    sleep 0.2
done
