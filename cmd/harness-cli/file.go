//go:build !js

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/on-keyday/agent-harness/cli"
)

// The `file edit` and `file new` bodies. They sat in main.go, which is where
// every verb's body sat before the dispatch split them out; these three were
// simply not carried across with the rest.

// runFileEdit pulls a worktree file, opens it in $EDITOR, and writes it back.
// A CLI has no terminal UI of its own to host an editor widget, so unlike the
// TUI this path always goes through an external editor.
func runFileEdit(ctx context.Context, c *cli.Client, taskID, rel string) error {
	doc, err := c.FileEditLoad(ctx, taskID, rel, nil)
	if err != nil {
		return err
	}
	edited, tmp, err := editViaExternalEditor(rel, doc.Text)
	if err != nil {
		return err
	}
	for force := false; ; force = true {
		st, cerr := c.FileEditCommit(ctx, taskID, doc, edited, force)
		if cerr != nil {
			return fmt.Errorf("%w (your edit is kept at %s)", cerr, tmp)
		}
		switch st {
		case cli.FileEditUnchanged:
			os.Remove(tmp)
			fmt.Printf("no change: %s\n", rel)
			return nil
		case cli.FileEditPushed:
			os.Remove(tmp)
			fmt.Printf("saved: %s\n", rel)
			return nil
		}
		fmt.Fprintf(os.Stderr, "%s changed on the runner since it was read. Overwrite? [y/N] ", rel)
		var answer string
		fmt.Fscanln(os.Stdin, &answer)
		if answer != "y" && answer != "Y" {
			fmt.Fprintf(os.Stderr, "not overwritten; your edit is kept at %s\n", tmp)
			return nil
		}
	}
}

// runFileNew opens an empty buffer in $EDITOR and pushes it to rel.
func runFileNew(ctx context.Context, c *cli.Client, taskID, rel string) error {
	text, tmp, err := editViaExternalEditor(rel, "")
	if err != nil {
		return err
	}
	if err := c.FilePushBytes(ctx, taskID, []byte(text), rel, cli.FilePushOpts{MkdirParents: true}, nil); err != nil {
		return fmt.Errorf("%w (your text is kept at %s)", err, tmp)
	}
	os.Remove(tmp)
	fmt.Printf("created: %s\n", rel)
	return nil
}

// editViaExternalEditor spools text to a temp file, runs $EDITOR on it with
// this process's stdio, and returns the result. The temp path comes back too
// so callers can name it when a later step fails and the edit would otherwise
// be lost.
func editViaExternalEditor(name, text string) (string, string, error) {
	f, err := os.CreateTemp("", "harness-edit-*"+filepath.Ext(name))
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", "", err
	}
	f.Close()
	cmd, err := cli.ExternalEditorCommand(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", tmp, fmt.Errorf("editor exited with an error: %w (your text is kept at %s)", err, tmp)
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		return "", tmp, err
	}
	return string(b), tmp, nil
}
