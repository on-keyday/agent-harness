package agent

import (
	"flag"
	"strings"
	"testing"
)

// parseForPayload builds the flag set both publish verbs share and parses args
// through it, so these cases exercise the same fs.Visit / fs.Args() state the
// real commands hand to resolvePayload.
func parseForPayload(t *testing.T, args []string) (*flag.FlagSet, string) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	data := fs.String("data", "-", "")
	fs.String("topic", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return fs, *data
}

func TestResolvePayload_Sources(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		stdin      string
		wantBody   string
		wantSource string
	}{
		{
			name:       "explicit --data is a literal body",
			args:       []string{"--topic", "t", "--data", "hello"},
			wantBody:   "hello",
			wantSource: sourceData,
		},
		{
			name:       "positional words are joined ssh-style",
			args:       []string{"--topic", "t", "hello", "world"},
			wantBody:   "hello world",
			wantSource: sourcePositional,
		},
		{
			name:       "--data - reads stdin",
			args:       []string{"--topic", "t", "--data", "-"},
			stdin:      "piped\n",
			wantBody:   "piped\n",
			wantSource: sourceStdin,
		},
		{
			name:       "neither --data nor a positional reads stdin",
			args:       []string{"--topic", "t"},
			stdin:      "piped",
			wantBody:   "piped",
			wantSource: sourceStdin,
		},
		{
			// The misread this reporting exists for: `--data D|-` in the usage
			// text read as a positional produces `send --topic T -`, which is a
			// successful publish of the one-byte body "-". Nothing about the
			// send fails, so `source: positional` is the only tell.
			name:       "a bare - positional is a one-byte body, NOT stdin",
			args:       []string{"--topic", "t", "-"},
			stdin:      "this must not be read",
			wantBody:   "-",
			wantSource: sourcePositional,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, data := parseForPayload(t, tc.args)
			body, source, err := resolvePayload(fs, data, strings.NewReader(tc.stdin))
			if err != nil {
				t.Fatalf("resolvePayload: %v", err)
			}
			if string(body) != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}
