package cli

// Environment names of the operator-only secret: read by operator surfaces
// (GetOperatorPSK) and taken by harness-server as the value of --operator-psk.
// Named once so that the runner can drop exactly these before it spawns an
// agent (cmd/agent-runner) and the launch scripts can keep them out of the
// runner (scripts/daemon.py) without a spelling of their own.
const (
	OperatorPSKEnv     = "HARNESS_OPERATOR_PSK"
	OperatorPSKFileEnv = "HARNESS_OPERATOR_PSK_FILE"
)
