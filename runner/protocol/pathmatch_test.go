package protocol

import "testing"

// RootMatches is the one roots predicate (G9): a task's repo must EQUAL one of
// a runner's roots, after path.Clean on both. Containment no longer counts.
func TestRootMatches(t *testing.T) {
	cases := []struct {
		name string
		root string
		repo string
		want bool
	}{
		{"equal", "/home/u/ws", "/home/u/ws", true},
		{"child refused", "/home/u/ws", "/home/u/ws/foo", false},
		{"parent refused", "/home/u/ws", "/home/u", false},
		{"sibling lookalike", "/home/u/ws", "/home/u/ws-evil", false},
		{"trailing slash root", "/a/b/", "/a/b", true},
		{"trailing slash repo", "/a/b", "/a/b/", true},
		{"redundant slashes", "/a/b", "/a//b", true},
		{"relative repo refused", "/a/b", "a/b", false},
		{"relative root refused", "a/b", "/a/b", false},
		{"posix root serves only itself", "/", "/anything", false},
		{"posix root vs itself", "/", "/", true},
		{"win equal", "C:/Users/foo", "C:/Users/foo", true},
		{"win child refused", "C:/Users/foo", "C:/Users/foo/bar", false},
		{"win drive case not normalised", "C:/w", "c:/w", false},
		{"win different drive", "C:/w", "D:/w", false},
		{"win drive without slash refused", "C:w", "C:w", false},
		{"cross-form mismatch", "/Users/foo", "C:/Users/foo", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RootMatches(tc.root, tc.repo); got != tc.want {
				t.Fatalf("RootMatches(%q,%q)=%v want %v", tc.root, tc.repo, got, tc.want)
			}
		})
	}
}
