package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/sshgw"
)

// SSHGatewaySession is the ssh listener this TUI hosts. There is at most one:
// unlike a forward it is not per-task, so there is nothing to key a second one
// by and no way for the operator to tell two apart.
type SSHGatewaySession struct {
	Listen string
	Cancel context.CancelFunc
}

// SSHGatewayStartedMsg registers a listener that is already bound.
type SSHGatewayStartedMsg struct {
	Listen string
	Cancel context.CancelFunc
}

// SSHGatewayStoppedMsg removes it again, whether it was stopped on purpose or
// its Serve loop failed.
type SSHGatewayStoppedMsg struct{}

// SSHGatewayStatusMsg carries a line to append to cmdresult.
type SSHGatewayStatusMsg struct{ Line string }

// DoStartSSHGateway binds the listener and, once it is actually bound, starts
// serving in the background. Uses the long-lived client (NOT a fresh dial),
// like every other Do* in this layer.
//
// The bind happens here rather than inside the goroutine so a failure — a port
// already in use, a non-loopback address with no authorized-keys file — is
// reported as a failure. Announcing "started" first and contradicting it a
// moment later is the shape DoStartRemoteForward exists to avoid.
func DoStartSSHGateway(c *cli.Client, listen, hostKeyPath, authKeysPath string, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		gw, err := sshgw.Listen(c, sshgw.Options{
			Listen:             listen,
			HostKeyPath:        hostKeyPath,
			AuthorizedKeysPath: authKeysPath,
		})
		if err != nil {
			// Every error out of Listen already opens with "ssh-gateway:".
			return SSHGatewayStatusMsg{Line: ErrorStyle.Render("✗ ") + err.Error()}
		}
		ctx, cancel := context.WithCancel(context.Background())
		addr := gw.Addr()
		// Sent before the goroutine starts so it is enqueued ahead of any
		// Stopped the serve loop might produce immediately.
		program.Send(SSHGatewayStartedMsg{Listen: addr, Cancel: cancel})
		go func() {
			defer gw.Close()
			// Serve ends on its own when the harness connection dies, not only
			// when the operator stops it, so this is a real path: the Stopped
			// below is what takes the listener off `ssh-gateway status`, which
			// otherwise kept advertising an address whose client was gone.
			//
			// No "ssh-gateway:" prefix here — every error Serve returns already
			// opens with it.
			if serr := gw.Serve(ctx); serr != nil {
				program.Send(SSHGatewayStatusMsg{Line: ErrorStyle.Render("✗ ") + serr.Error()})
			}
			program.Send(SSHGatewayStoppedMsg{})
		}()
		return nil
	}
}

// sshGatewayStartedLines is what the operator needs after a successful start:
// the address, and the invocation to paste into ~/.ssh/config.
func sshGatewayStartedLines(addr string) []string {
	return []string{
		OKStyle.Render("ssh-gateway listening on ") + addr,
		"  ssh -p " + sshgw.PortOf(addr) + " <32-hex-task-id>@" + sshgw.HostOf(addr) +
			"   (bare = cowrite; .control takes the seat; .view watches)",
		"  Ctrl+] detaches — ssh's own ~. disconnects instead and leaves your terminal's modes unreset",
		"  ssh -L / -W tunnel through it too: the runner dials, each connection shows in `forward ls`",
	}
}

// runSSHGatewayAction handles the `ssh-gateway` command line verb. Returns nil
// when there is nothing to dispatch — the report has already been appended.
func (a *App) runSSHGatewayAction(v SSHGatewayAction) tea.Cmd {
	switch v.Sub {
	case "status":
		if a.sshGateway == nil {
			a.cmdresult.Append("ssh-gateway: not running (`ssh-gateway start` to listen)")
			return nil
		}
		for _, line := range sshGatewayStartedLines(a.sshGateway.Listen) {
			a.cmdresult.Append(line)
		}
		return nil

	case "start":
		if a.sshGateway != nil {
			a.cmdresult.Append(ErrorStyle.Render("ssh-gateway: already listening on ") + a.sshGateway.Listen +
				" — stop it first (`ssh-gateway stop`)")
			return nil
		}
		if a.client == nil {
			a.cmdresult.Append(ErrorStyle.Render("ssh-gateway: not connected to server"))
			return nil
		}
		// No --authorized-keys equivalent here: the TUI's gateway is the
		// same-machine case, and a non-loopback address without keys is refused
		// by sshgw.Listen with a message that says so.
		return DoStartSSHGateway(a.client, v.Listen, sshgw.DefaultHostKeyPath(a.configPath), "", a.program)

	case "stop":
		if a.sshGateway == nil {
			a.cmdresult.Append(ErrorStyle.Render("ssh-gateway: not running"))
			return nil
		}
		a.sshGateway.Cancel()
		a.cmdresult.Append("ssh-gateway: stopping " + a.sshGateway.Listen)
		return nil
	}
	return nil
}
