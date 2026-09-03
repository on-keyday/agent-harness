package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/objtrsf/objproto"
)

func workspaceUsage() { workspaceUsageTo(os.Stderr) }

func workspaceUsageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli workspace save <name> [--task <32-hex>] [--resume no|continue|fresh] [--runner assigned|any] [--repo PATH]")
	fmt.Fprintln(w, "       harness-cli workspace rm <name>")
	fmt.Fprintln(w, "       harness-cli workspace ls")
	fmt.Fprintln(w, "       harness-cli workspace show [<name>]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "save records every task the registry reports a forward for; --task narrows it")
	fmt.Fprintln(w, "to one, and is also how a task's forwards get CLEARED after you stop them.")
	fmt.Fprintln(w, "It MERGES: task blocks it did not observe are kept, and an existing block's")
	fmt.Fprintln(w, "resume / runner are never overwritten — those are yours to hand-edit.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "There is no `workspace apply` here: a forward lives exactly as long as the")
	fmt.Fprintln(w, "client holding its control stream, so a short-lived CLI process could only")
	fmt.Fprintln(w, "establish one by staying in the foreground — which is `harness-cli forward`.")
	fmt.Fprintln(w, "The TUI applies a workspace on start, on reconnect, and on `workspace apply`.")
}

// runWorkspace implements the `workspace` family.
//
// save reads the task's forwards from the server-side REGISTRY, the only source
// a short-lived CLI process has: it holds no forwards of its own. That registry
// also carries forwards other clients established, so a CLI save can capture
// more than a TUI save would — see the spec's "Writing a workspace". Filtering
// by origin_cid is not the fix: this process owns none of them.
// runWorkspaceAction backs the four workspace verbs the CLI declares. It
// reads the config path and the resolved server-cid from the process-level
// values main() installed, because a verb method carries the ACTION and
// nothing about how this process was invoked.
func runWorkspaceAction(ctx context.Context, a verb.WorkspaceAction, cid func() objproto.ConnectionID) error {
	f, path, err := workspace.Load(workspaceCfgPath)
	if err != nil {
		return err
	}
	if path == "" {
		path = workspace.DefaultPath
	}

	switch a.Sub {
	case "ls":
		// Arity is the declaration's now: `workspace ls` takes no positional,
		// so a stray word is refused at the parse rather than by a len(args)
		// test written per sub-verb.
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
		if f == nil || !f.Remove(a.Name) {
			return fmt.Errorf("no workspace named %q in %s", a.Name, path)
		}
		if err := f.Save(path); err != nil {
			return err
		}
		fmt.Printf("workspace %s removed from %s\n", a.Name, path)
		return nil

	case "show":
		if f == nil {
			return fmt.Errorf("no config at %s", path)
		}
		if a.Name == "" {
			for _, n := range f.Names() {
				ws, _ := f.Workspace(n)
				os.Stdout.Write(workspace.Block(ws))
			}
			return nil
		}
		ws, ok := f.Workspace(a.Name)
		if !ok {
			return fmt.Errorf("no workspace named %q in %s", a.Name, path)
		}
		os.Stdout.Write(workspace.Block(ws))
		return nil

	case "save":
		// `ws` is taken below by the workspace.Workspace being built.
		name := a.Name
		taskID, resume, runner, repo := &a.TaskID, &a.Resume, &a.Runner, &a.Repo

		// An empty filter lists every forward the caller may see, so a bare
		// `workspace save <name>` records the same set the TUI would rather than
		// one task. --task narrows it, and is also what lets a save CLEAR one
		// task's forwards: the registry reports presence, never absence.
		forwards, err := cli.PortForwardList(ctx, cid(), *taskID)
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

		ws := &workspace.Workspace{Name: name, ServerCID: workspaceServerCIDStr, Repo: *repo}
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
	return fmt.Errorf("workspace: unhandled sub-verb %q", a.Sub)
}

// workspaceCfgPath and workspaceServerCIDStr are what `workspace save` needs
// from the invocation rather than from the verb: which config file to write
// and the server-cid string to record in it. Installed by main() once, for
// the reason verb.EnvLookup is -- a dispatch method takes the action and
// nothing else, and threading two process-level strings through 74 signatures
// to reach one of them is worse than naming them here.
var (
	workspaceCfgPath      string
	workspaceServerCIDStr string
)

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
