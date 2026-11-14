package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/exec/frame"
)

// A-D33: OpenExec's no_pty runs the child on pipes. A oneshot's stdin is
// /dev/null; an interactive session's is the client's input.

// sentFrames splits what the runner wrote to the session stream into frames,
// keeping the Stdout and Stderr payloads apart.
func sentFrames(t *testing.T, r oneshotRun) (stdout, stderr []byte) {
	t.Helper()
	b, _ := r.stream.sentBytes()
	rd := bytes.NewReader(b)
	for rd.Len() > 0 {
		f := &frame.Frame{}
		if err := f.Read(rd); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		d := f.Data()
		if d == nil {
			continue
		}
		switch f.Header.Type {
		case frame.FrameType_Stdout:
			stdout = append(stdout, *d...)
		case frame.FrameType_Stderr:
			stderr = append(stderr, *d...)
		}
	}
	return stdout, stderr
}

func stdinFrame(b []byte) []byte {
	hdr := frame.FrameHeader{Type: frame.FrameType_Stdin, Len: uint32(len(b))}
	return append(hdr.MustAppend(nil), b...)
}

// A no-PTY oneshot reads /dev/null: `cat` sees EOF at once rather than waiting
// on a terminal nobody types into. Its output reaches the stream byte for byte
// — bare LF, nothing a terminal would add — with stdout and stderr apart, and
// no pager env, since nothing is a terminal to page on.
func TestNoPtyOneshotReadsDevNullAndIsByteExact(t *testing.T) {
	for _, k := range []string{"PAGER", "GIT_PAGER", "SYSTEMD_PAGER"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	argv := []string{"-c", `cat; printf 'a\nb\n'; printf '%s|%s' "${PAGER-unset}" "${GIT_PAGER-unset}" >&2`, "sh", "{prompt}"}
	start := time.Now()
	r := openExecOpts(t, AgentProfile{Name: "default", Bin: "/bin/sh", OneshotArgv: argv},
		execOpts{oneshot: true, noPTY: true, prompt: "p"}, nil)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the oneshot took %v: its stdin was not at EOF", d)
	}
	if tf := r.finished(t); tf.ExitCode != 0 {
		t.Fatalf("exit %d (%q)", tf.ExitCode, tf.ErrorMessage)
	}
	stdout, stderr := sentFrames(t, r)
	if string(stdout) != "a\nb\n" {
		t.Fatalf("stdout = %q, want the bytes the child wrote", stdout)
	}
	if string(stderr) != "unset|unset" {
		t.Fatalf("stderr = %q, want its own frames and no pager env", stderr)
	}
}

// A no-PTY interactive session's stdin is a pipe fed from the client: a line
// the client sends reaches the child, and nothing echoes it back.
func TestNoPtyInteractiveReceivesClientStdin(t *testing.T) {
	argv := []string{"-c", `read line; printf 'got:%s\n' "$line"`, "sh"}
	r := openExecOpts(t, AgentProfile{Name: "default", Bin: "/bin/sh", InteractiveArgv: argv},
		execOpts{noPTY: true, input: stdinFrame([]byte("hello\n"))}, nil)
	if tf := r.finished(t); tf.ExitCode != 0 {
		t.Fatalf("exit %d (%q)", tf.ExitCode, tf.ErrorMessage)
	}
	if stdout, _ := sentFrames(t, r); string(stdout) != "got:hello\n" {
		t.Fatalf("stdout = %q, want the child's line and no terminal echo", stdout)
	}
}

// A child on pipes gets no agentboard wake, interactive or not: with no
// terminal it is a program reading its input stream, and the marker would be
// input to it (bash on a pipe exits on the marker's syntax error). A no-PTY
// oneshot has /dev/null besides. An interactive PTY session does get one.
func TestNoPtyWake(t *testing.T) {
	for _, tc := range []struct {
		name     string
		oneshot  bool
		noPTY    bool
		wantWake bool
	}{
		{"oneshot", true, true, false},
		{"interactive", false, true, false},
		{"interactive with a PTY", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "gate")
			argv := gatedSh(gate)
			var hasWake bool
			openExecOpts(t, AgentProfile{Name: "default", Bin: "/bin/sh", OneshotArgv: argv, InteractiveArgv: argv},
				execOpts{oneshot: tc.oneshot, noPTY: tc.noPTY, prompt: "p"}, func(s *Session, id string) {
					s.mu.Lock()
					e, _ := s.reg.get(id)
					hasWake = e.wake != nil
					s.mu.Unlock()
					if err := os.WriteFile(gate, nil, 0o600); err != nil {
						t.Error(err)
					}
				})
			if hasWake != tc.wantWake {
				t.Fatalf("wake installed = %v, want %v", hasWake, tc.wantWake)
			}
		})
	}
}
