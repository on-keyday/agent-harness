package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func testKey(b byte) ed25519.PublicKey {
	k := make([]byte, ed25519.PublicKeySize)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestFormatRunnerEnroll(t *testing.T) {
	k := testKey(0xab)
	if got := FormatRunnerEnroll(protocol.RunnerKeyStatus_Ok, k, "gmk"); got != "enrolled abababababababab label=gmk" {
		t.Errorf("ok: %q", got)
	}
	if got := FormatRunnerEnroll(protocol.RunnerKeyStatus_AlreadyEnrolled, k, ""); got != "already enrolled abababababababab" {
		t.Errorf("already: %q", got)
	}
}

// Zero is a measurement: a key nobody uses prints runners=0, and the legacy
// line is printed even when it is empty.
func TestFormatRunnerKeysNeverElidesZero(t *testing.T) {
	rows := []RunnerKeyRow{
		{Key: testKey(1), Label: "gmk", EnrolledAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC), EnrolledBy: "operator",
			Runners: []RunnerKeyUser{{ID: protocol.RunnerID{Id: [16]byte{0xaa, 0xbb, 0xcc, 0xdd}}, Hostname: "gmkhost"}}},
		{Key: testKey(2), Label: "", EnrolledAt: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC), EnrolledBy: "task:00ff"},
	}
	got := FormatRunnerKeys(rows, nil)
	for _, want := range []string{
		"0101010101010101 label=gmk enrolled=2026-10-27T09:00:00Z by=operator runners=1",
		"  aabbccdd gmkhost",
		"0202020202020202 label= enrolled=2026-10-27T10:00:00Z by=task:00ff runners=0",
		"legacy runners=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatRunnerUnenroll(t *testing.T) {
	k := testKey(0xcd)
	res := RunnerUnenrollResult{Key: k, Label: "gmk", Runners: []RunnerKeyUser{{ID: protocol.RunnerID{Id: [16]byte{1, 2, 3, 4}}, Hostname: "gmkhost"}}}

	got, ok := FormatRunnerUnenroll(protocol.RunnerKeyStatus_InUse, res)
	if ok || !strings.Contains(got, "refused: 1 runner(s) use cdcdcdcdcdcdcdcd (pass --force to cut them off):") || !strings.Contains(got, "  01020304 gmkhost") {
		t.Errorf("in_use: ok=%v\n%s", ok, got)
	}
	got, ok = FormatRunnerUnenroll(protocol.RunnerKeyStatus_Ok, res)
	undo := "undo: harness-cli runner enroll " + hex.EncodeToString(k) + " --label gmk"
	if !ok || !strings.Contains(got, "unenrolled cdcdcdcdcdcdcdcd label=gmk, closed 1 runner(s)") || !strings.Contains(got, undo) {
		t.Errorf("ok: ok=%v\n%s", ok, got)
	}
	if _, ok := FormatRunnerUnenroll(protocol.RunnerKeyStatus_NotFound, RunnerUnenrollResult{}); ok {
		t.Error("not_found must not read as success")
	}
}
