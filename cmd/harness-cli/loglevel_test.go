package main

import (
	"log/slog"
	"testing"
)

// The default is the point of the flag: a one-shot command must not print the
// connection layers' INFO trail unless asked.
func TestParseLogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":      slog.LevelWarn,
		"info":  slog.LevelInfo,
		"DEBUG": slog.LevelDebug,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		got, err := parseLogLevel(in)
		if err != nil || got != want {
			t.Errorf("parseLogLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseLogLevel("loud"); err == nil {
		t.Error(`parseLogLevel("loud") accepted an unknown level`)
	}
}
