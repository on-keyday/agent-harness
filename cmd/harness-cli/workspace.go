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
	fmt.Fprintln(os.Stderr, "usage: harness-cli workspace save <name> [--task <32-hex>] [--resume no|continue|fresh] [--runner assigned|any] [--repo PATH]")
	fmt.Fprintln(os.Stderr, "       harness-cli workspace rm <name>")
	fmt.Fprintln(os.Stderr, "       harness-cli workspace ls")
	fmt.Fprintln(os.Stderr, "       harness-cli workspace show [<name>]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "save records every task the registry reports a forward for; --task narrows it")
	fmt.Fprintln(os.Stderr, "to one, and is also how a task's forwards get CLEARED after you stop them.")
	fmt.Fprintln(os.Stderr, "It MERGES: task blocks it did not observe are kept, and an existing block's")
	fmt.Fprintln(os.Stderr, "resume / runner are never overwritten — those are yours to hand-edit.")
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

	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("workspace rm: exactly one name")
		}
		if f == nil || !f.Remove(args[1]) {
			return fmt.Errorf("no workspace named %q in %s", args[1], path)
		}
		if err := f.Save(path); err != nil {
			return err
		}
		fmt.Printf("workspace %s removed from %s\n", args[1], path)
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
		taskID := fs.String("task", "", "record only this task (32 hex); omitted = every task the registry reports a forward for")
		resume := fs.String("resume", string(workspace.ResumeContinue), "no | continue | fresh — for a task block being written for the FIRST time")
		runner := fs.String("runner", string(workspace.RunnerAssigned), "assigned | any — for a task block being written for the FIRST time")
		repo := fs.String("repo", "", "repo identifier to record in the workspace")
		fs.Parse(args[2:])
		if *taskID != "" {
			if _, err := hex.DecodeString(*taskID); err != nil || len(*taskID) != 32 {
				return fmt.Errorf("workspace save: --task must be a 32-hex task id, got %q", *taskID)
			}
		}

		// An empty filter lists every forward the caller may see, so a bare
		// `workspace save <name>` records the same set the TUI would rather than
		// one task. --task narrows it, and is also what lets a save CLEAR one
		// task's forwards: the registry reports presence, never absence.
		forwards, err := cli.PortForwardList(ctx, cid, *taskID)
		if err != nil {
			return err
		}
		byTask := map[string]*workspace.Task{}
		var order []string
		skipped := 0
		for i := range forwards {
			spec, ok := cli.PortForwardConfigSpec(&forwards[i])
			if !ok {
				skipped++ // in-process: no local address to write down
				continue
			}
			id := hex.EncodeToString(forwards[i].TaskId.Id[:])
			t, seen := byTask[id]
			if !seen {
				t = &workspace.Task{ID: id, Resume: workspace.Resume(*resume), Runner: workspace.Runner(*runner)}
				byTask[id] = t
				order = append(order, id)
			}
			t.Forwards = append(t.Forwards, spec)
		}
		observed := map[string]bool{}
		for _, id := range order {
			observed[id] = true
		}
		if *taskID != "" {
			observed[*taskID] = true // named but forward-less: clear its forwards
		}

		ws := &workspace.Workspace{Name: name, ServerCID: serverCIDStr, Repo: *repo}
		for _, id := range order {
			ws.Tasks = append(ws.Tasks, *byTask[id])
		}
		// Validate here rather than trusting the flags: --resume / --runner are
		// free-form strings at this layer, and an unknown one must be refused
		// before it reaches the file, not on the next client's start-up.
		for _, t := range ws.Tasks {
			if err := validateWorkspaceEnums(t); err != nil {
				return err
			}
		}
		if err := ws.Validate(); err != nil {
			return err
		}
		if f == nil {
			f = workspace.New()
		}
		existing, _ := f.Workspace(name)
		ws = workspace.Merge(existing, ws, observed)
		f.Set(ws)
		if err := f.Save(path); err != nil {
			return err
		}
		fmt.Printf("workspace %s saved to %s: %d task(s), %d forward(s), %d in-process skipped\n",
			name, path, len(ws.Tasks), countTaskForwards(ws), skipped)
		return nil
	}
	workspaceUsage()
	os.Exit(2)
	return nil
}

func countTaskForwards(ws *workspace.Workspace) int {
	n := 0
	for _, t := range ws.Tasks {
		n += len(t.Forwards)
	}
	return n
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
