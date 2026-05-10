package runner

import "testing"

func TestValidateRelPath(t *testing.T) {
	cases := []struct {
		name    string
		root    string
		rel     string
		wantOK  bool
		wantOut string // expected joined absolute path; "" if !wantOK
	}{
		{"ok plain", "/wt", "foo.txt", true, "/wt/foo.txt"},
		{"ok subdir", "/wt", "a/b/c.txt", true, "/wt/a/b/c.txt"},
		{"ok empty (root)", "/wt", "", true, "/wt"},
		{"reject absolute", "/wt", "/etc/passwd", false, ""},
		{"reject parent", "/wt", "../escape", false, ""},
		{"reject embedded parent", "/wt", "a/../../escape", false, ""},
		{"reject NUL", "/wt", "a\x00b", false, ""},
		{"reject leading dotdot after clean", "/wt", "./../x", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateRelPath(tc.root, tc.rel)
			if tc.wantOK && err != nil {
				t.Fatalf("expected ok, got err: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("expected error, got ok with path=%q", got)
			}
			if tc.wantOK && got != tc.wantOut {
				t.Errorf("path mismatch: got %q want %q", got, tc.wantOut)
			}
		})
	}
}
