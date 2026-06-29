#!/usr/bin/env bash
# Fake claude binary that emits one line wrapped in SGR escapes (red), then
# stays alive. Used by the raw-snapshot integration test: the raw byte stream
# must preserve the ESC[31m..ESC[0m sequences, while the VT-rendered snapshot
# flattens them to plain "REDLINE".
printf '\033[31mREDLINE\033[0m\n'
sleep 60
