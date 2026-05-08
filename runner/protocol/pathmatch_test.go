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
		{"posix root covers any abs", "/", "/anything/here", true},
		{"posix root vs itself", "/", "/", true},

		// Windows drive-letter forms (runner emits these via filepath.ToSlash).
		{"win exact", "C:/Users/foo", "C:/Users/foo", true},
		{"win child", "C:/Users/foo", "C:/Users/foo/bar", true},
		{"win deep child", "C:/Users/foo", "C:/Users/foo/bar/baz", true},
		{"win sibling lookalike", "C:/Users/foo", "C:/Users/foo-evil", false},
		{"win different drive", "C:/Users/foo", "D:/Users/foo", false},
		{"win parent", "C:/Users/foo", "C:/Users", false},
		{"win lowercase drive", "c:/users/foo", "c:/users/foo/bar", true},
		{"win drive only refused (no slash)", "C:Users/foo", "C:Users/foo/bar", false},
		{"win cross-form mismatch", "/Users/foo", "C:/Users/foo", false},
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

// MatchLen is the specificity score backing longest-prefix-match runner
// selection. 0 means no match; non-zero is len(path.Clean(root)) so deeper
// roots score higher and shadow shallower fallbacks.
func TestMatchLen(t *testing.T) {
	cases := []struct {
		name string
		root string
		repo string
		want int
	}{
		{"no match", "/a", "/b/c", 0},
		{"sibling lookalike scores 0", "/a/b", "/a/b-evil", 0},
		{"relative refused", "a/b", "/a/b/c", 0},

		{"exact match scores root len", "/a/b", "/a/b", 4},
		{"child scores root len, not repo len", "/a/b", "/a/b/c/d", 4},
		{"deeper root scores higher", "/a/b/c", "/a/b/c/d", 6},
		{"posix root scores 1", "/", "/anything/here", 1},

		{"trailing slash on root collapses", "/a/b/", "/a/b/c", 4},
		{"redundant slashes in repo collapse", "/a/b", "/a//b//c", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchLen(tc.root, tc.repo)
			if got != tc.want {
				t.Fatalf("MatchLen(%q,%q)=%d want %d", tc.root, tc.repo, got, tc.want)
			}
		})
	}
}
