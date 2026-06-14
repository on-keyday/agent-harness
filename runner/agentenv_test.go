package runner

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func mustParseCID(t *testing.T, s string) objproto.ConnectionID {
	t.Helper()
	cid, err := objproto.ParseConnectionID(s, 0)
	if err != nil {
		t.Fatal(err)
	}
	return cid
}

func envMap(env []string) map[string]string {
	out := make(map[string]string)
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 {
			out[e[:i]] = e[i+1:]
		}
	}
	return out
}

func TestBuildAgentEnv_AllFields(t *testing.T) {
	var taskID protocol.TaskID
	copy(taskID.Id[:], []byte{0xde, 0xad, 0xbe, 0xef})
	var ticket [16]byte
	copy(ticket[:], []byte{0xfe, 0xed, 0xfa, 0xce})

	spec := AgentEnvSpec{
		ServerCID:  mustParseCID(t, "ws:127.0.0.1:8539-12345"),
		RunnerID:   mustParseCID(t, "ws:1.2.3.4:9999-42"),
		TaskID:     taskID,
		RepoPath:   "/home/u/repo",
		Hostname:   "dev-pi-01",
		WSPath:     "/ws",
		AuthTicket: ticket,
	}
	env := BuildAgentEnv(spec)
	want := map[string]string{
		"HARNESS_SERVER_CID":  spec.ServerCID.String(),
		"HARNESS_RUNNER_ID":   spec.RunnerID.String(),
		"HARNESS_TASK_ID":     hex.EncodeToString(taskID.Id[:]),
		"HARNESS_REPO_PATH":   "/home/u/repo",
		"HARNESS_HOSTNAME":    "dev-pi-01",
		"HARNESS_WS_PATH":     "/ws",
		"HARNESS_AUTH_TICKET": hex.EncodeToString(ticket[:]),
	}
	got := envMap(env)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestBuildAgentEnv_OmitsEmptyHostname(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
		WSPath:    "/ws",
	}
	env := BuildAgentEnv(spec)
	for _, e := range env {
		if strings.HasPrefix(e, "HARNESS_HOSTNAME=") {
			t.Errorf("hostname should be omitted when empty, got %q", e)
		}
	}
}

func TestBuildAgentEnv_BinDirPrependsPATH(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
		WSPath:    "/ws",
		BinDir:    "/opt/harness/bin",
	}
	env := BuildAgentEnv(spec)
	got := envMap(env)
	want := "/opt/harness/bin" + string(os.PathListSeparator) + "/usr/bin:/bin"
	if got["PATH"] != want {
		t.Errorf("PATH = %q, want %q", got["PATH"], want)
	}
}

func TestBuildAgentEnv_BinDirEmpty_NoPATHEntry(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
		WSPath:    "/ws",
	}
	env := BuildAgentEnv(spec)
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			t.Errorf("PATH should be omitted when BinDir empty, got %q", e)
		}
	}
}

func TestBuildAgentEnv_PSKForwarded(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
		WSPath:    "/ws",
		PSK:       []byte("hunter2"),
	}
	got := envMap(BuildAgentEnv(spec))
	if got["HARNESS_PSK"] != "hunter2" {
		t.Errorf("HARNESS_PSK = %q, want %q", got["HARNESS_PSK"], "hunter2")
	}
}

func TestBuildAgentEnv_PSKEmpty_NoEntry(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
		WSPath:    "/ws",
	}
	env := BuildAgentEnv(spec)
	for _, e := range env {
		if strings.HasPrefix(e, "HARNESS_PSK=") {
			t.Errorf("HARNESS_PSK should be omitted when PSK nil, got %q", e)
		}
	}
}

// TestBuildAgentEnv_DisablesMingwPathConv verifies that MSYS_NO_PATHCONV=1
// and MSYS2_ARG_CONV_EXCL=* are injected so that when claude runs under
// MSYS/MinGW bash on Windows, POSIX-style paths passed as args (e.g. "/ws"
// or topic strings like "chat/demo") are not silently rewritten into
// Windows paths. Harmless no-op on non-Windows / non-MSYS shells.
func TestBuildAgentEnv_DisablesMingwPathConv(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
		WSPath:    "/ws",
	}
	got := envMap(BuildAgentEnv(spec))
	if got["MSYS_NO_PATHCONV"] != "1" {
		t.Errorf("MSYS_NO_PATHCONV = %q, want %q", got["MSYS_NO_PATHCONV"], "1")
	}
	if got["MSYS2_ARG_CONV_EXCL"] != "*" {
		t.Errorf("MSYS2_ARG_CONV_EXCL = %q, want %q", got["MSYS2_ARG_CONV_EXCL"], "*")
	}
}

func TestBuildAgentEnv_BinDirWithEmptyParentPATH(t *testing.T) {
	t.Setenv("PATH", "")
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
		WSPath:    "/ws",
		BinDir:    "/opt/harness/bin",
	}
	env := BuildAgentEnv(spec)
	got := envMap(env)
	if got["PATH"] != "/opt/harness/bin" {
		t.Errorf("PATH = %q, want %q (no separator when parent PATH empty)", got["PATH"], "/opt/harness/bin")
	}
}

func TestBuildAgentEnvIncludesProxyVia(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:127.0.0.1:8540-2"),
		ProxyVia:  "ws:127.0.0.1:8540-*",
	}
	env := BuildAgentEnv(spec)
	want := "HARNESS_PROXY_VIA_RUNNER=ws:127.0.0.1:8540-*"
	found := false
	for _, e := range env {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("env missing %q; got %v", want, env)
	}
}

func TestBuildAgentEnvOmitsProxyViaWhenEmpty(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:127.0.0.1:8540-2"),
		// ProxyVia intentionally empty
	}
	env := BuildAgentEnv(spec)
	for _, e := range env {
		if strings.HasPrefix(e, "HARNESS_PROXY_VIA_RUNNER=") {
			t.Errorf("env should not contain HARNESS_PROXY_VIA_RUNNER, got %q", e)
		}
	}
}

// TestRewriteProxyViaForLocalDial covers the bind-addr → loopback rewrite
// applied to HARNESS_PROXY_VIA_RUNNER. Listen-side bind addresses
// (0.0.0.0, ::, empty host) must become loopback (127.0.0.1, ::1) so the
// agent — same host as the runner — can dial portably (Linux accepts
// 0.0.0.0 dial as localhost, Windows / macOS do not).
func TestRewriteProxyViaForLocalDial(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Bind-only IPv4 → loopback
		{"ws:0.0.0.0:8540-*", "ws:127.0.0.1:8540-*"},
		// Empty host (--listen :8540) → loopback
		{"ws::8540-*", "ws:127.0.0.1:8540-*"},
		// IPv6 unspecified → loopback v6
		{"ws:[::]:8540-*", "ws:[::1]:8540-*"},
		// Already loopback IPv4 → unchanged
		{"ws:127.0.0.1:8540-*", "ws:127.0.0.1:8540-*"},
		// Concrete IP → unchanged
		{"ws:192.168.3.14:8540-*", "ws:192.168.3.14:8540-*"},
		// Concrete IPv6 → unchanged
		{"ws:[fe80::1]:8540-*", "ws:[fe80::1]:8540-*"},
		// Hostname → unchanged (DNS resolves at dial time)
		{"ws:proxy-runner.local:8540-*", "ws:proxy-runner.local:8540-*"},
		// UDP transport variant — same rewrite rules
		{"udp:0.0.0.0:8541-*", "udp:127.0.0.1:8541-*"},
		{"udp::8541-*", "udp:127.0.0.1:8541-*"},
		// Random-ID suffix preserved (lifetime-bound conn_id, not "*")
		{"ws:0.0.0.0:8540-12345", "ws:127.0.0.1:8540-12345"},
	}
	for _, c := range cases {
		got := rewriteProxyViaForLocalDial(c.in)
		if got != c.want {
			t.Errorf("rewriteProxyViaForLocalDial(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestBuildAgentEnvAppliesRewrite confirms the env-injection path actually
// passes ProxyVia through the rewrite (regression guard).
func TestBuildAgentEnvAppliesRewrite(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:9001-1"),
		RunnerID:  mustParseCID(t, "ws:127.0.0.1:9002-2"),
		TaskID:    protocol.TaskID{},
		ProxyVia:  "ws:0.0.0.0:8540-*",
	}
	env := BuildAgentEnv(spec)
	want := "HARNESS_PROXY_VIA_RUNNER=ws:127.0.0.1:8540-*"
	found := false
	for _, e := range env {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("env did not contain rewritten %q; full env:\n%s", want, strings.Join(env, "\n"))
	}
}

func TestBuildAgentEnv_X11(t *testing.T) {
	env := BuildAgentEnv(AgentEnvSpec{
		ServerCID:   mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:    mustParseCID(t, "ws:1.2.3.4:9999-1"),
		X11Enabled:  true,
		X11Display:  10,
		X11AuthFile: "/tmp/harness-xauth-abc",
	})
	var gotDisplay, gotXauth bool
	for _, e := range env {
		if e == "DISPLAY=127.0.0.1:10" {
			gotDisplay = true
		}
		if e == "XAUTHORITY=/tmp/harness-xauth-abc" {
			gotXauth = true
		}
	}
	if !gotDisplay || !gotXauth {
		t.Fatalf("missing X11 env: display=%v xauth=%v in %v", gotDisplay, gotXauth, env)
	}
}

func TestBuildAgentEnv_X11NoAuth(t *testing.T) {
	env := BuildAgentEnv(AgentEnvSpec{
		X11Enabled: true,
		X11Display: 10,
		// no X11AuthFile
	})
	var gotDisplay, gotXauth bool
	for _, e := range env {
		if e == "DISPLAY=127.0.0.1:10" {
			gotDisplay = true
		}
		if len(e) >= 11 && e[:11] == "XAUTHORITY=" {
			gotXauth = true
		}
	}
	if !gotDisplay {
		t.Fatalf("DISPLAY missing in no-auth mode: %v", env)
	}
	if gotXauth {
		t.Fatalf("XAUTHORITY must NOT be set in no-auth mode: %v", env)
	}
}

func TestBuildAgentEnv_NoX11WhenUnset(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
	}
	for _, e := range BuildAgentEnv(spec) {
		if len(e) >= 8 && e[:8] == "DISPLAY=" {
			t.Fatalf("unexpected DISPLAY when X11 unset: %q", e)
		}
	}
}
