package connauth

import (
	"os/exec"
	"strings"
	"testing"
)

// The daemons build no operator-surface code: package cli (and the verb
// table it pulls in) is what the CLI, TUI and WebUI run, and its handshake,
// signature and reconnect half moved here so that neither daemon imports it.
// Before the move every cli/ change, a new verb row included, changed both
// daemon binaries and land.py's `go list -deps` hint told the operator to
// restart them. One import of cli from server/ or runner/ brings that back,
// so it fails here. cli/cliopts (the HARNESS_* env resolution the handshake
// reads) is the one package under cli/ a daemon may build.
func TestDaemonsDoNotBuildPackageCli(t *testing.T) {
	const mod = "github.com/on-keyday/agent-harness"
	allowed := map[string]bool{mod + "/cli/cliopts": true}
	out, err := exec.Command("go", "list", "-deps", "../cmd/harness-server", "../cmd/agent-runner").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if (pkg == mod+"/cli" || strings.HasPrefix(pkg, mod+"/cli/")) && !allowed[pkg] {
			t.Errorf("a daemon builds %s: every change there would read as a daemon restart (land.py); move what the daemon needs into connauth", pkg)
		}
	}
}
