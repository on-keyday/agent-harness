#!/bin/bash
# Fake claude binary for tests. Echoes the prompt to stdout, writes one line to stderr,
# creates a file (so a follow-up `git status` would see a diff), and exits 0.
set -e
echo "stdout: prompt=$*"
echo "stderr line" >&2
touch hello.txt
exit 0
