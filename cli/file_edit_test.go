package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNewFileEditDocLFRoundTrip(t *testing.T) {
	orig := []byte("alpha\nbeta\n")
	d, err := newFileEditDoc("notes.txt", orig)
	if err != nil {
		t.Fatalf("newFileEditDoc: %v", err)
	}
	if d.CRLF {
		t.Errorf("CRLF=true, want false for an LF-only file")
	}
	if d.Text != "alpha\nbeta\n" {
		t.Errorf("Text=%q, want %q", d.Text, "alpha\nbeta\n")
	}
	if got := d.Encode(d.Text); !bytes.Equal(got, orig) {
		t.Errorf("Encode round trip=%q, want %q", got, orig)
	}
}

func TestNewFileEditDocCRLFRoundTrip(t *testing.T) {
	orig := []byte("alpha\r\nbeta\r\n")
	d, err := newFileEditDoc("notes.txt", orig)
	if err != nil {
		t.Fatalf("newFileEditDoc: %v", err)
	}
	if !d.CRLF {
		t.Fatalf("CRLF=false, want true for a CRLF-only file")
	}
	if d.Text != "alpha\nbeta\n" {
		t.Errorf("Text=%q, want LF-normalized %q", d.Text, "alpha\nbeta\n")
	}
	if got := d.Encode(d.Text); !bytes.Equal(got, orig) {
		t.Errorf("Encode round trip=%q, want %q", got, orig)
	}
	// An edit that adds a line keeps CRLF for the new line too.
	if got, want := d.Encode("alpha\nbeta\ngamma\n"), []byte("alpha\r\nbeta\r\ngamma\r\n"); !bytes.Equal(got, want) {
		t.Errorf("Encode(added line)=%q, want %q", got, want)
	}
}

// Mixed line endings normalize to LF. This is the spec's one deliberate lossy
// case; the test pins it so changing the rule fails loudly instead of quietly.
func TestNewFileEditDocMixedEndingsNormalizeToLF(t *testing.T) {
	d, err := newFileEditDoc("mixed.txt", []byte("alpha\r\nbeta\ngamma\r\n"))
	if err != nil {
		t.Fatalf("newFileEditDoc: %v", err)
	}
	if d.CRLF {
		t.Errorf("CRLF=true, want false when a bare LF is present")
	}
	if got, want := d.Encode(d.Text), []byte("alpha\nbeta\ngamma\n"); !bytes.Equal(got, want) {
		t.Errorf("Encode=%q, want %q", got, want)
	}
}

func TestNewFileEditDocBOMRoundTrip(t *testing.T) {
	orig := append([]byte{0xEF, 0xBB, 0xBF}, []byte("alpha\n")...)
	d, err := newFileEditDoc("bom.txt", orig)
	if err != nil {
		t.Fatalf("newFileEditDoc: %v", err)
	}
	if !d.BOM {
		t.Fatalf("BOM=false, want true")
	}
	if d.Text != "alpha\n" {
		t.Errorf("Text=%q, want the BOM stripped", d.Text)
	}
	if got := d.Encode(d.Text); !bytes.Equal(got, orig) {
		t.Errorf("Encode round trip=%q, want %q", got, orig)
	}
}

func TestNewFileEditDocBOMOnlyFile(t *testing.T) {
	orig := []byte{0xEF, 0xBB, 0xBF}
	d, err := newFileEditDoc("bom-only.txt", orig)
	if err != nil {
		t.Fatalf("newFileEditDoc: %v", err)
	}
	if d.Text != "" {
		t.Errorf("Text=%q, want empty", d.Text)
	}
	if got := d.Encode(""); !bytes.Equal(got, orig) {
		t.Errorf("Encode round trip=%q, want the lone BOM back", got)
	}
}

func TestNewFileEditDocRejectsInvalidUTF8(t *testing.T) {
	_, err := newFileEditDoc("bin", []byte{'o', 'k', 0xFF, 0xFE, 'x'})
	if !errors.Is(err, ErrFileEditNotText) {
		t.Errorf("err=%v, want ErrFileEditNotText", err)
	}
}

func TestNewFileEditDocRejectsNUL(t *testing.T) {
	// Valid UTF-8, but a NUL byte means this is not something a human edits.
	_, err := newFileEditDoc("bin", []byte("PK\x03\x04\x00\x00text"))
	if !errors.Is(err, ErrFileEditNotText) {
		t.Errorf("err=%v, want ErrFileEditNotText", err)
	}
}

func TestNewFileEditDocSizeBoundary(t *testing.T) {
	atLimit := bytes.Repeat([]byte("a"), FileEditMaxBytes)
	if _, err := newFileEditDoc("big.txt", atLimit); err != nil {
		t.Errorf("exactly at the limit rejected: %v", err)
	}
	over := bytes.Repeat([]byte("a"), FileEditMaxBytes+1)
	_, err := newFileEditDoc("big.txt", over)
	if !errors.Is(err, ErrFileEditTooLarge) {
		t.Errorf("err=%v, want ErrFileEditTooLarge", err)
	}
}

// The editor widget may hand back CRLF of its own (an external editor on
// Windows). The doc's flag alone must decide what goes on the wire.
func TestEncodeCollapsesEditorSuppliedCRLF(t *testing.T) {
	d, err := newFileEditDoc("notes.txt", []byte("alpha\n"))
	if err != nil {
		t.Fatalf("newFileEditDoc: %v", err)
	}
	if got, want := d.Encode("alpha\r\nbeta\r\n"), []byte("alpha\nbeta\n"); !bytes.Equal(got, want) {
		t.Errorf("Encode=%q, want %q", got, want)
	}
}

func TestFileEditDecide(t *testing.T) {
	orig := []byte("alpha\n")
	cases := []struct {
		name   string
		next   []byte
		remote []byte
		force  bool
		want   FileEditStatus
	}{
		{"identical bytes are unchanged", []byte("alpha\n"), orig, false, FileEditUnchanged},
		{"remote untouched pushes", []byte("beta\n"), orig, false, FileEditPushed},
		{"remote moved conflicts", []byte("beta\n"), []byte("agent wrote this\n"), false, FileEditConflict},
		{"force overrides a moved remote", []byte("beta\n"), []byte("agent wrote this\n"), true, FileEditPushed},
		{"unchanged wins over force", []byte("alpha\n"), []byte("agent wrote this\n"), true, FileEditUnchanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileEditDecide(orig, tc.next, tc.remote, tc.force); got != tc.want {
				t.Errorf("fileEditDecide=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestExternalEditorCommandPrefersVisual(t *testing.T) {
	t.Setenv("VISUAL", "myvisual")
	t.Setenv("EDITOR", "myeditor")
	cmd, err := ExternalEditorCommand("/tmp/x.txt")
	if err != nil {
		t.Fatalf("ExternalEditorCommand: %v", err)
	}
	if !strings.HasSuffix(cmd.Args[0], "myvisual") {
		t.Errorf("Args[0]=%q, want myvisual", cmd.Args[0])
	}
	if cmd.Args[len(cmd.Args)-1] != "/tmp/x.txt" {
		t.Errorf("last arg=%q, want the path", cmd.Args[len(cmd.Args)-1])
	}
}

func TestExternalEditorCommandSplitsFlags(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "code -w")
	cmd, err := ExternalEditorCommand("/tmp/x.txt")
	if err != nil {
		t.Fatalf("ExternalEditorCommand: %v", err)
	}
	if got, want := cmd.Args[1:], []string{"-w", "/tmp/x.txt"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Args[1:]=%v, want %v", got, want)
	}
}

func TestExternalEditorCommandUnset(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if _, err := ExternalEditorCommand("/tmp/x.txt"); !errors.Is(err, ErrNoExternalEditor) {
		t.Errorf("err=%v, want ErrNoExternalEditor", err)
	}
}
