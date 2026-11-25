//go:build !js

package cli

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/connauth"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// WhatIfActor is the task a what-if from this process is made by: this
// process's own task (agent or credential identity) or "" for the operator.
// A task's re-grant is predicted under the task-caller rule (delegated-regrant
// spec R9), so the preview matches what caps set will answer.
func WhatIfActor() string {
	kind, tid := connauth.EffectiveClientKind(protocol.ClientKind_Cli)
	if kind != protocol.ClientKind_Agent && kind != protocol.ClientKind_Credential {
		return ""
	}
	return hex.EncodeToString(tid.Id[:])
}
