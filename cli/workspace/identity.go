package workspace

import (
	"errors"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ForwardKey is what makes two forward values the same forward: the direction
// and the parsed spec, never its spelling. `-L 3000`, the long form it expands
// to, and what a save renders from the registry are one key.
//
// For -R, BindPort is the runner's port and Target* is what the client dials.
type ForwardKey struct {
	Dir        ForwardDir
	BindAddr   string
	BindPort   int
	TargetHost string
	TargetPort int
	Protocol   protocol.ForwardProtocol
}

// ForwardIdentity parses a `forward` value into its key. Every comparison of a
// declared value against anything else goes through here, so a spelling
// difference can never read as a different forward.
func ForwardIdentity(value string) (ForwardKey, error) {
	dir, l, r, err := ParseForwardValue(value)
	if err != nil {
		return ForwardKey{}, err
	}
	if dir == ForwardRemote {
		return ForwardKey{Dir: dir, BindAddr: r.BindAddr, BindPort: r.RunnerPort,
			TargetHost: r.DialHost, TargetPort: r.DialPort, Protocol: r.Protocol}, nil
	}
	return ForwardKey{Dir: dir, BindAddr: l.BindAddr, BindPort: l.LocalPort,
		TargetHost: l.RemoteHost, TargetPort: l.RemotePort, Protocol: l.Protocol}, nil
}

// Value renders the key through cli.PortForwardConfigSpec — the function a save
// renders the registry with — so a value written from a typed spec and one
// written from a listing are the same string.
func (k ForwardKey) Value() (string, error) {
	if k.BindPort == 0 {
		return "", errors.New("local port 0 is chosen at bind time and cannot be kept")
	}
	dir := protocol.PortForwardDirection_Local
	if k.Dir == ForwardRemote {
		dir = protocol.PortForwardDirection_Remote
	}
	v, ok := cli.PortForwardConfigSpec(&protocol.PortForwardInfo{
		Direction: dir, ClientEndpoint: protocol.ClientEndpointKind_OsSocket,
		BindAddr: []byte(k.BindAddr), BindPort: uint16(k.BindPort),
		TargetHost: []byte(k.TargetHost), TargetPort: uint16(k.TargetPort),
		Protocol: k.Protocol,
	})
	if !ok {
		return "", errors.New("forward cannot be written down")
	}
	return v, nil
}

func (ws *Workspace) task(taskID string) *Task {
	for i := range ws.Tasks {
		if ws.Tasks[i].ID == taskID {
			return &ws.Tasks[i]
		}
	}
	return nil
}

// Declares reports whether the task's block declares this forward.
func (ws *Workspace) Declares(taskID, value string) bool {
	k, err := ForwardIdentity(value)
	if err != nil {
		return false
	}
	t := ws.task(taskID)
	return t != nil && declaresKey(t.Forwards, k)
}

func declaresKey(values []string, k ForwardKey) bool {
	for _, v := range values {
		if got, err := ForwardIdentity(v); err == nil && got == k {
			return true
		}
	}
	return false
}

// AddForward declares value for the task, creating its block with the policy a
// save gives a first-time block. An identical forward already declared is not
// added twice. The value is written as given.
func (ws *Workspace) AddForward(taskID, value string) (bool, error) {
	k, err := ForwardIdentity(value)
	if err != nil {
		return false, err
	}
	t := ws.task(taskID)
	if t == nil {
		ws.Tasks = append(ws.Tasks, Task{ID: taskID, Resume: ResumeContinue, Runner: RunnerAssigned})
		t = &ws.Tasks[len(ws.Tasks)-1]
	}
	if declaresKey(t.Forwards, k) {
		return false, nil
	}
	t.Forwards = append(t.Forwards, value)
	return true, nil
}

// RemoveForward drops every declaration of value from the task's block. The
// block stays even when no forward is left: its resume/runner are a separate
// statement about the task.
func (ws *Workspace) RemoveForward(taskID, value string) (bool, error) {
	k, err := ForwardIdentity(value)
	if err != nil {
		return false, err
	}
	t := ws.task(taskID)
	if t == nil {
		return false, nil
	}
	kept := t.Forwards[:0]
	removed := false
	for _, v := range t.Forwards {
		if got, err := ForwardIdentity(v); err == nil && got == k {
			removed = true
			continue
		}
		kept = append(kept, v)
	}
	t.Forwards = kept
	return removed, nil
}
