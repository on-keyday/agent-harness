#!/bin/bash
# Fake claude that records its stdin to $WAKE_OUT and stays alive until killed.
# Used by integration/wake_e2e_test.go to assert the wake marker reaches the
# process via runner->PTY/pipe stdin injection.
#
# Implementation note: previous versions used `read -r line` which waits for
# '\n'. The runner's wake mechanism (runner/session.go::WakeStdin) sends the
# marker text first then a lone '\r' as the submit keystroke (tuned for
# Ink-based TUIs that interpret CR as Enter on PTY input). On a regular
# pipe, '\r' is NOT a line terminator, so `read -r` never returns and the
# marker is silently buffered — making the wake test perpetually time out.
#
# `cat - >>` copies bytes through unbuffered (write(2) syscalls per chunk),
# so partial writes appear in WAKE_OUT immediately regardless of newlines.
# The trap on SIGTERM/SIGHUP/SIGINT still fires because we keep cat as a
# child (no `exec`), and `wait $!` lets the trap handler get scheduled.
set -e
echo "stdout: prompt=$*"
WAKE_OUT="${WAKE_OUT:-/tmp/wake.out}"
: > "$WAKE_OUT"
# exec replaces this shell with cat so cat keeps the parent's stdin (the
# runner's stdinPipeR). cat copies bytes directly via write(2) without
# stdio buffering on the output fd opened by `>>`, so each chunk the
# runner writes appears in WAKE_OUT immediately. Signals kill cat
# directly; the test relies on file content already on disk by then.
exec cat - >> "$WAKE_OUT"
