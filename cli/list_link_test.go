package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func linkedRunner(link protocol.RunnerLink, record uint32) protocol.RunnerInfo {
	r := makeRunnerInfo("cloud", protocol.RunnerStatus_Idle, 2, []string{"/r"}, 0)
	r.Link, r.LinkRecord = link, record
	return r
}

func TestListRunnerRowShowsLink(t *testing.T) {
	for _, render := range []func(*protocol.ListResultBody, interface{ Write([]byte) (int, error) }){
		func(lr *protocol.ListResultBody, w interface{ Write([]byte) (int, error) }) { renderList(lr, w) },
		func(lr *protocol.ListResultBody, w interface{ Write([]byte) (int, error) }) { renderListTree(lr, w) },
	} {
		lr := &protocol.ListResultBody{Runners: []protocol.RunnerInfo{
			linkedRunner(protocol.RunnerLink_DialRecord, 0x3fa9c012),
			linkedRunner(protocol.RunnerLink_DialedIn, 0),
		}}
		var out strings.Builder
		render(lr, &out)
		s := out.String()
		if !strings.Contains(s, "link=dial:3fa9c012") || !strings.Contains(s, "link=in") {
			t.Errorf("runner rows lack link=:\n%s", s)
		}
	}
}

func TestListRunnerJSONHasLinkFields(t *testing.T) {
	via := linkedRunner(protocol.RunnerLink_Via, 0)
	via.LinkVia.Id[0] = 0xab
	lr := &protocol.ListResultBody{Runners: []protocol.RunnerInfo{linkedRunner(protocol.RunnerLink_DialRecord, 0x3fa9c012), via}}
	var out strings.Builder
	renderListJSON(lr, &out)
	var doc struct {
		Runners []map[string]any `json:"runners"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if r := doc.Runners[0]; r["link"] != "dial_record" || r["link_record"] != "3fa9c012" || r["link_via"] != "" {
		t.Errorf("record runner: %v", r)
	}
	if r := doc.Runners[1]; r["link"] != "via" || r["link_record"] != "" || !strings.HasPrefix(r["link_via"].(string), "ab") || len(r["link_via"].(string)) != 32 {
		t.Errorf("via runner: %v", r)
	}
}
