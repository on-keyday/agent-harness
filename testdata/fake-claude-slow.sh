#!/bin/bash
# Fake claude binary for cancel/disconnect tests. Sleeps for a long time so
# the test can cancel or disconnect while it is "running".
set -e
echo "stdout: slow claude starting, prompt=$*"
sleep 60
echo "stdout: slow claude done"
exit 0
