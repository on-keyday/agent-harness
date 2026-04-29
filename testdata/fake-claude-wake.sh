#!/bin/bash
# Fake claude that records its stdin to $WAKE_OUT and stays alive until killed.
# Used by integration/wake_e2e_test.go to assert the wake marker reaches the
# process via runner->PTY/pipe stdin injection.
set -e
echo "stdout: prompt=$*"
trap 'exit 0' SIGTERM SIGHUP SIGINT
: > "${WAKE_OUT:-/tmp/wake.out}"
while IFS= read -r line; do
	echo "$line" >> "${WAKE_OUT:-/tmp/wake.out}"
done
