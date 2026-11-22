package server

import (
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// requiredCapOf is the bit the central gate checks for k, zero when it checks
// none: the single-value read the tests made of the map before it moved into
// authority.
func requiredCapOf(k protocol.TaskControlKind) protocol.Capability {
	c, _ := authority.CentralCap(k)
	return c
}
