package cli

import (
	"context"
	"fmt"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"io"
)

// FileLs prints one line per entry under taskIDHex/<relPath> to out.
// Format: "<mode-octal> <size> <name>[/]" (trailing slash for directories).
func (c *Client) FileLs(ctx context.Context, taskIDHex, relPath string, route protocol.FileTransferRoute, out io.Writer) error {
	entries, err := c.ListFiles(ctx, taskIDHex, relPath, route)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name
		if e.IsDir {
			name += "/"
		}
		size := e.Size
		if e.IsDir {
			size = 0
		}
		if _, err := fmt.Fprintf(out, "%04o %10d %s\n", e.Mode&0o7777, size, name); err != nil {
			return err
		}
	}
	return nil
}
