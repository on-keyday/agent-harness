//go:build !js

package cli

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func runnerInfoWith(label string, key byte, legacy bool) protocol.RunnerInfo {
	r := protocol.RunnerInfo{MaxTasks: 1}
	r.SetHostname([]byte("gmkhost"))
	if key != 0 {
		for i := range r.Key {
			r.Key[i] = key
		}
	}
	r.SetKeyLabel([]byte(label))
	r.SetLegacyAuth(legacy)
	return r
}

func TestRunnerKeyStr(t *testing.T) {
	for _, c := range []struct {
		r    protocol.RunnerInfo
		want string
	}{
		{runnerInfoWith("gmk", 0x11, false), "gmk"},
		{runnerInfoWith("", 0x11, false), "1111111111111111"},
		{runnerInfoWith("", 0, true), "legacy"},
	} {
		if got := RunnerKeyStr(c.r); got != c.want {
			t.Errorf("RunnerKeyStr = %q, want %q", got, c.want)
		}
	}
}

func TestRunnerRowShowsKeyLabelOrLegacy(t *testing.T) {
	var out bytes.Buffer
	renderList(&protocol.ListResultBody{Runners: []protocol.RunnerInfo{runnerInfoWith("gmk", 0x11, false), runnerInfoWith("", 0, true)}}, &out)
	if !strings.Contains(out.String(), "key=gmk") || !strings.Contains(out.String(), "key=legacy") {
		t.Fatalf("runner rows lack key=:\n%s", out.String())
	}
	out.Reset()
	renderListTree(&protocol.ListResultBody{Runners: []protocol.RunnerInfo{runnerInfoWith("gmk", 0x11, false)}}, &out)
	if !strings.Contains(out.String(), "key=gmk") {
		t.Fatalf("tree runner rows lack key=:\n%s", out.String())
	}
}

func TestRunnerJSONCarriesKeyFields(t *testing.T) {
	var out bytes.Buffer
	renderListJSON(&protocol.ListResultBody{Runners: []protocol.RunnerInfo{runnerInfoWith("gmk", 0x11, false), runnerInfoWith("", 0, true)}}, &out)
	s := out.String()
	for _, want := range []string{
		`"key":"` + hex.EncodeToString(bytes.Repeat([]byte{0x11}, 32)) + `"`,
		`"key_label":"gmk"`, `"legacy_auth":false`, `"legacy_auth":true`, `"key":""`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ls --json lacks %s:\n%s", want, s)
		}
	}
}

func TestConnRowMarksLegacy(t *testing.T) {
	ci := &protocol.ConnInfo{Role: protocol.ConnRole_Runner}
	ci.SetCid([]byte("ws:127.0.0.1:8539-1"))
	ci.SetIdentified(true)
	ci.SetLegacyAuth(true)
	if line := connInfoTextLine(ci); !strings.Contains(line, "legacy") {
		t.Fatalf("conn row does not mark legacy: %q", line)
	}
	if j := newConnInfoJSON(ci); !j.LegacyAuth {
		t.Fatal("conn JSON lacks legacy_auth=true")
	}
	ci.SetLegacyAuth(false)
	if line := connInfoTextLine(ci); strings.Contains(line, "legacy") {
		t.Fatalf("a non-legacy conn row reads legacy: %q", line)
	}
}
