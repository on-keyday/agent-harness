package cli

import "testing"

// A bad spec must be rejected before anything is dialled: an operator who
// mistyped a header should not first watch a forward be established and
// registered, then torn down.
func TestRunHTTPRequestForwardValidatesBeforeDial(t *testing.T) {
	cases := map[string]HTTPRequestSpec{
		"relative path": {Path: "relative"},
		"bad header":    {Path: "/", Headers: []string{"no-colon"}},
		"crlf in path":  {Path: "/x\r\nX: 1"},
	}
	for name, spec := range cases {
		// A nil *Client is safe precisely because validation runs first; if it
		// ever stops running first this dereferences and the test fails loudly.
		if err := RunHTTPRequestForward(t.Context(), nil, "task", "h", 1, spec, nil, nil); err == nil {
			t.Errorf("%s: want a validation error, got nil", name)
		}
	}
}
