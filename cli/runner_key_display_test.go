//go:build !js

package cli

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func runnerInfoWith(label string, key byte) protocol.RunnerInfo {
	r := protocol.RunnerInfo{MaxTasks: 1}
	r.SetHostname([]byte("gmkhost"))
	if key != 0 {
		for i := range r.Key {
			r.Key[i] = key
		}
	}
	r.SetKeyLabel([]byte(label))
	return r
}

func TestRunnerKeyStr(t *testing.T) {
	for _, c := range []struct {
		r    protocol.RunnerInfo
		want string
	}{
		{runnerInfoWith("gmk", 0x11), "gmk"},
		{runnerInfoWith("", 0x11), "1111111111111111"},
	} {
		if got := RunnerKeyStr(c.r); got != c.want {
			t.Errorf("RunnerKeyStr = %q, want %q", got, c.want)
		}
	}
}

func TestRunnerRowShowsKeyLabelOrPrefix(t *testing.T) {
	var out bytes.Buffer
	renderList(&protocol.ListResultBody{Runners: []protocol.RunnerInfo{runnerInfoWith("gmk", 0x11), runnerInfoWith("", 0x22)}}, &out)
	if !strings.Contains(out.String(), "key=gmk") || !strings.Contains(out.String(), "key=2222222222222222") {
		t.Fatalf("runner rows lack key=:\n%s", out.String())
	}
	out.Reset()
	renderListTree(&protocol.ListResultBody{Runners: []protocol.RunnerInfo{runnerInfoWith("gmk", 0x11)}}, &out)
	if !strings.Contains(out.String(), "key=gmk") {
		t.Fatalf("tree runner rows lack key=:\n%s", out.String())
	}
}

func TestRunnerJSONCarriesKeyFields(t *testing.T) {
	var out bytes.Buffer
	renderListJSON(&protocol.ListResultBody{Runners: []protocol.RunnerInfo{runnerInfoWith("gmk", 0x11), runnerInfoWith("", 0x22)}}, &out)
	s := out.String()
	for _, want := range []string{
		`"key":"` + hex.EncodeToString(bytes.Repeat([]byte{0x11}, 32)) + `"`,
		`"key_label":"gmk"`, `"key_label":""`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ls --json lacks %s:\n%s", want, s)
		}
	}
}
