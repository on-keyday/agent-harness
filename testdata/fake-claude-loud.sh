#!/usr/bin/env bash
# Emit ~5 MiB to overflow the default 1 MiB ring buffer.
# Used by integration tests to verify ring buffer wrap-around behavior.
yes "loud line $(date -u +%s.%N)" | head -c 5000000
echo
sleep 2
