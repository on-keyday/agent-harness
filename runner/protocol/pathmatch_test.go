package protocol

import "testing"

func TestIsUnderRoot(t *testing.T) {
	cases := []struct {
		name string
		root string
		repo string
		want bool
	}{
		{"exact match", "/home/kforfk/workspace", "/home/kforfk/workspace", true},
		{"child", "/home/kforfk/workspace", "/home/kforfk/workspace/foo", true},
		{"deep child", "/home/kforfk/workspace", "/home/kforfk/workspace/foo/bar", true},
		{"sibling lookalike", "/home/kforfk/workspace", "/home/kforfk/workspace-evil", false},
		{"unrelated", "/home/kforfk/workspace", "/etc/passwd", false},
		{"trailing slash root", "/home/kforfk/workspace/", "/home/kforfk/workspace/foo", true},
		{"trailing slash repo", "/home/kforfk/workspace", "/home/kforfk/workspace/foo/", true},
		{"relative repo refused", "/home/kforfk/workspace", "workspace/foo", false},
		{"relative root refused", "workspace", "/home/kforfk/workspace/foo", false},
		{"root parent", "/home/kforfk/workspace", "/home/kforfk", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsUnderRoot(tc.root, tc.repo)
			if got != tc.want {
				t.Fatalf("IsUnderRoot(%q,%q)=%v want %v", tc.root, tc.repo, got, tc.want)
			}
		})
	}
}
