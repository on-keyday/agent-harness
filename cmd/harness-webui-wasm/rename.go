//go:build js && wasm

package main

import (
	"bytes"
	"strings"
	"syscall/js"

	"github.com/on-keyday/agent-harness/cli"
)

// harnessTaskRename is session rename (delegated-regrant spec R3) over the
// long-lived client: taskRename(id, name) resolves to the result line. An
// empty name clears the task's name.
func harnessTaskRename(this js.Value, args []js.Value) any {
	id, name := argString(args, 0), argString(args, 1)
	return credentialPromise(func() (any, error) { return nil, nil }, func(c *cli.Client, _ any) (any, error) {
		var out bytes.Buffer
		if err := cli.TaskRenameWith(rootCtx, c, id, name, &out); err != nil {
			return nil, err
		}
		return strings.TrimRight(out.String(), "\n"), nil
	})
}
