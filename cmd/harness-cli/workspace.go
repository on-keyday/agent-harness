package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/objtrsf/objproto"
)

func workspaceUsage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli workspace save <name> --task <32-hex> [--resume no|continue|fresh] [--runner assigned|any] [--repo PATH]")
	fmt.Fprintln(os.Stderr, "       harness-cli workspace ls")
	fmt.Fprintln(os.Stderr, "       harness-cli workspace show [<name>]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "There is no `workspace apply` here: a forward lives exactly as long as the")
	fmt.Fprintln(os.Stderr, "client holding its control stream, so a short-lived CLI process could only")
	fmt.Fprintln(os.Stderr, "establish one by staying in the foreground — which is `harness-cli forward`.")
	fmt.Fprintln(os.Stderr, "The TUI applies a workspace on start, on reconnect, and on `workspace apply`.")
}

// runWorkspace implements the `workspace` family.
//
// save reads the task's forwards from the server-side REGISTRY, the only source
// a short-lived CLI process has: it holds no forwards of its own. That registry
// also carries forwards other clients established, so a CLI save can capture
// more than a TUI save would — see the spec's "Writing a workspace". Filtering
// by origin_cid is not the fix: this process owns none of them.
func runWorkspace(ctx context.Context, args []string, cid objproto.ConnectionID, cfgPath, serverCIDStr string) error {
	if len(args) == 0 {
		workspaceUsage()
		os.Exit(2)
	}
	f, path, err := workspace.Load(cfgPath)
	if err != nil {
		return err
	}
	if path == "" {
		path = workspace.DefaultPath
	}

	switch args[0] {
	case "ls":
		if len(args) != 1 {
			return fmt.Errorf("workspace ls: takes no arguments")
		}
		names := f.Names()
		if len(names) == 0 {
			fmt.Printf("no workspaces in %s\n", path)
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil

	case "show":
		if len(args) > 2 {
			return fmt.Errorf("workspace show: at most one name")
		}
		if f == nil {
			return fmt.Errorf("no config at %s", path)
		}
		if len(args) == 1 {
			for _, n := range f.Names() {
				ws, _ := f.Workspace(n)
				os.Stdout.Write(workspace.Block(ws))
			}
			return nil
		}
		ws, ok := f.Workspace(args[1])
		if !ok {
			return fmt.Errorf("no workspace named %q in %s", args[1], path)
		}
		os.Stdout.Write(workspace.Block(ws))
		return nil

	case "save":
		if len(args) < 2 {
			workspaceUsage()
			os.Exit(2)
		}
		name := args[1]
		fs := flag.NewFlagSet("workspace save", flag.ExitOnError)
		taskID := fs.String("task", "", "task id (32 hex) whose forwards to capture")
		resume := fs.String("resume", string(workspace.ResumeContinue), "no | continue | fresh")
		runner := fs.String("runner", string(workspace.RunnerAssigned), "assigned | any")
		repo := fs.String("repo", "", "repo identifier to record in the workspace")
		fs.Parse(args[2:])
		if _, err := hex.DecodeString(*taskID); err != nil || len(*taskID) != 32 {
			return fmt.Errorf("workspace save: --task must be a 32-hex task id, got %q", *taskID)
		}

		forwards, err := cli.PortForwardList(ctx, cid, *taskID)
		if err != nil {
			return err
		}
		tk := workspace.Task{ID: *taskID, Resume: workspace.Resume(*resume), Runner: workspace.Runner(*runner)}
		skipped := 0
		for i := range forwards {
			spec, ok := cli.PortForwardConfigSpec(&forwards[i])
			if !ok {
				skipped++ // in-process: no local address to write down
				continue
			}
			tk.Forwards = append(tk.Forwards, spec)
		}

		ws := &workspace.Workspace{Name: name, ServerCID: serverCIDStr, Repo: *repo, Tasks: []workspace.Task{tk}}
		// Validate here rather than trusting the flags: --resume / --runner are
		// free-form strings at this layer, and an unknown one must be refused
		// before it reaches the file, not on the next client's start-up.
		if err := ws.Validate(); err != nil {
			return err
		}
		if err := validateWorkspaceEnums(tk); err != nil {
			return err
		}
		if f == nil {
			f = workspace.New()
		}
		f.Set(ws)
		if err := f.Save(path); err != nil {
			return err
		}
		fmt.Printf("workspace %s saved to %s: 1 task, %d forward(s), %d in-process skipped\n",
			name, path, len(tk.Forwards), skipped)
		return nil
	}
	workspaceUsage()
	os.Exit(2)
	return nil
}

// validateWorkspaceEnums rejects a --resume / --runner value the file's grammar
// does not accept. Workspace.Validate checks the VALUES a workspace carries
// (forward specs, the grid string) but not these two, because a parsed file
// cannot hold a bad one — only this flag path can produce it.
func validateWorkspaceEnums(tk workspace.Task) error {
	switch tk.Resume {
	case workspace.ResumeNo, workspace.ResumeContinue, workspace.ResumeFresh:
	default:
		return fmt.Errorf("workspace save: --resume %q (want no, continue or fresh)", tk.Resume)
	}
	switch tk.Runner {
	case workspace.RunnerAssigned, workspace.RunnerAny:
	default:
		return fmt.Errorf("workspace save: --runner %q (want assigned or any)", tk.Runner)
	}
	return nil
}
