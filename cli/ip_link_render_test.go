package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ipRow is a link row the way the server lists one: 1 KiB each way, the
// datagram group at zero.
func ipRow(t *testing.T) *protocol.PortForwardInfo {
	t.Helper()
	cfg, err := ParseIPLink("10.200.0.1/30", "10.200.0.2")
	if err != nil {
		t.Fatal(err)
	}
	fi := &protocol.PortForwardInfo{ForwardId: 4, Direction: protocol.PortForwardDirection_Link,
		Protocol: protocol.ForwardProtocol_Ip, ClientEndpoint: protocol.ClientEndpointKind_OsSocket,
		BytesToTarget: 1024, BytesFromTarget: 1024}
	fi.SetLink(cfg)
	fi.SetCounter(protocol.ForwardCounterKey_MaxDatagramSize, 1169)
	fi.SetForwardDropCounters(protocol.ForwardHopRelay, protocol.ForwardDropsBody{})
	return fi
}

func TestPortForwardSpecStringIPLink(t *testing.T) {
	if got := PortForwardSpecString(ipRow(t)); got != "ip 10.200.0.1/30 <-> 10.200.0.2" {
		t.Fatalf("spec = %q", got)
	}
	noLink := &protocol.PortForwardInfo{Direction: protocol.PortForwardDirection_Link, Protocol: protocol.ForwardProtocol_Ip}
	if got := PortForwardSpecString(noLink); got != "ip (no link config)" {
		t.Fatalf("spec without a link = %q", got)
	}
}

func TestPortForwardDirFlagLink(t *testing.T) {
	if got := PortForwardDirFlag(protocol.PortForwardDirection_Link); got != "--ip" {
		t.Fatalf("dir flag = %q", got)
	}
	lines := PortForwardInfoLines([]protocol.PortForwardInfo{*ipRow(t)})
	header, row := lines[1], lines[2]
	if strings.Index(header, "SPEC") != strings.Index(row, "ip 10.200.0.1/30") {
		t.Fatalf("--ip pushed the columns out of line:\n%s\n%s", header, row)
	}
}

func TestTrafficLineIPLinkHasNoConns(t *testing.T) {
	line := PortForwardTrafficLine(ipRow(t))
	if strings.Contains(line, "conns=") {
		t.Fatalf("a link row reports connections: %q", line)
	}
	for _, want := range []string{"to-target=1.0kB", "from-target=1.0kB", "mtu=1169", "relay: oversize=0 congested=0 queued=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("traffic line %q lacks %q", line, want)
		}
	}
	tcp := &protocol.PortForwardInfo{Direction: protocol.PortForwardDirection_Local}
	if !strings.Contains(PortForwardTrafficLine(tcp), "conns=0/0") {
		t.Error("an idle tcp row lost its conns=0/0")
	}
}

func TestDatagramCellIPLink(t *testing.T) {
	if got := PortForwardDatagramCell(ipRow(t)); got != "1169 0/0/0" {
		t.Fatalf("datagram cell = %q", got)
	}
}

func TestConfigSpecRefusesLink(t *testing.T) {
	if spec, ok := PortForwardConfigSpec(ipRow(t)); ok {
		t.Fatalf("a link row is savable as %q", spec)
	}
}

func TestPortForwardJSONLink(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal([]byte(PortForwardInfoJSONLine(ipRow(t))), &got); err != nil {
		t.Fatal(err)
	}
	link, ok := got["link"].(map[string]any)
	if !ok {
		t.Fatalf("no link object: %v", got)
	}
	want := map[string]any{"client_addr": "10.200.0.1", "runner_addr": "10.200.0.2", "prefix_len": 30.0, "mtu": 1100.0}
	for k, v := range want {
		if link[k] != v {
			t.Errorf("link.%s = %v, want %v", k, link[k], v)
		}
	}
	tcp := &protocol.PortForwardInfo{Direction: protocol.PortForwardDirection_Local}
	if strings.Contains(PortForwardInfoJSONLine(tcp), `"link"`) {
		t.Error("a tcp row carries a link key")
	}
}

func TestForwardSnapshotRowLink(t *testing.T) {
	if _, ok := ForwardSnapshotRow(ipRow(t))["link"].(map[string]any); !ok {
		t.Fatal("the WebUI row lacks the link")
	}
	tcp := &protocol.PortForwardInfo{Direction: protocol.PortForwardDirection_Local}
	if _, ok := ForwardSnapshotRow(tcp)["link"]; ok {
		t.Fatal("a tcp WebUI row carries a link")
	}
}

func tapDatagramRec(flow uint32, dir protocol.ForwardTapDirection, seq uint64, fate protocol.ForwardTapDatagramFate, payload string) *protocol.ForwardTapRecord {
	d := protocol.ForwardTapDatagram{FlowId: flow, Direction: dir, Seq: seq, Fate: fate}
	d.SetData([]byte(payload))
	rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_Datagram, UnixMs: 1756000000000}
	rec.SetDatagram(d)
	return rec
}

func TestTapRendererDatagram(t *testing.T) {
	payload := strings.Repeat("x", 512)
	rec := tapDatagramRec(3, protocol.ForwardTapDirection_ToTarget, 41, protocol.ForwardTapDatagramFate_Relayed, payload)
	ts := tapTimestamp(rec.UnixMs)

	lines := NewTapRenderer(TapHex).Render(rec)
	if lines[0] != tapHeader("flow 3", "->", ts, "#41 relayed 512B") {
		t.Fatalf("header = %q", lines[0])
	}
	if len(lines) != 1+512/16 || !strings.HasPrefix(lines[1], "  00000000  78 78") {
		t.Fatalf("hex body = %d lines, first %q", len(lines)-1, lines[1])
	}
	if text := NewTapRenderer(TapText).Render(rec); len(text) != 2 || text[1] != "  "+payload {
		t.Fatalf("text = %q", text)
	}
	if raw := NewTapRenderer(TapRaw).Render(rec); len(raw) != 1 || raw[0] != payload {
		t.Fatalf("raw = %q", raw)
	}
	var j map[string]any
	if err := json.Unmarshal([]byte(NewTapRenderer(TapJSON).Render(rec)[0]), &j); err != nil {
		t.Fatal(err)
	}
	if j["kind"] != "datagram" || j["flow"] != 3.0 || j["seq"] != 41.0 || j["fate"] != "relayed" ||
		j["dir"] != "to_target" || j["len"] != 512.0 || j["truncated_bytes"] != 0.0 {
		t.Fatalf("json = %v", j)
	}

	drop := tapDatagramRec(3, protocol.ForwardTapDirection_FromTarget, 1, protocol.ForwardTapDatagramFate_DroppedCongestion, "y")
	if h := NewTapRenderer(TapHex).Render(drop)[0]; !strings.HasSuffix(h, "#1 dropped-congestion 1B") || !strings.Contains(h, "<-") {
		t.Fatalf("dropped header = %q", h)
	}
}

func TestTapRendererMissedDatagrams(t *testing.T) {
	r := NewTapRenderer(TapHex)
	r.Render(tapDatagramRec(3, protocol.ForwardTapDirection_ToTarget, 41, protocol.ForwardTapDatagramFate_Relayed, "a"))
	rec := tapDatagramRec(3, protocol.ForwardTapDirection_ToTarget, 49, protocol.ForwardTapDatagramFate_Relayed, "b")
	lines := r.Render(rec)
	if lines[0] != tapHeader("flow 3", "->", tapTimestamp(rec.UnixMs), "missed 7 datagrams") {
		t.Fatalf("missed line = %q", lines[0])
	}
	if !strings.Contains(lines[1], "#49 relayed") {
		t.Fatalf("the datagram's header did not follow: %q", lines[1])
	}
	next := r.Render(tapDatagramRec(3, protocol.ForwardTapDirection_ToTarget, 50, protocol.ForwardTapDatagramFate_Relayed, "c"))
	if strings.Contains(next[0], "missed") {
		t.Fatalf("a consecutive seq printed a jump: %q", next[0])
	}
	other := r.Render(tapDatagramRec(4, protocol.ForwardTapDirection_ToTarget, 9, protocol.ForwardTapDatagramFate_Relayed, "d"))
	back := r.Render(tapDatagramRec(3, protocol.ForwardTapDirection_FromTarget, 5, protocol.ForwardTapDatagramFate_Relayed, "e"))
	if strings.Contains(other[0], "missed") || strings.Contains(back[0], "missed") {
		t.Fatal("a first record on a flow or direction counted as a jump")
	}
}

func TestTapRendererDatagramGap(t *testing.T) {
	rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_DatagramGap, UnixMs: 1756000000000}
	rec.SetDatagramGap(protocol.ForwardTapDatagramGap{FlowId: 3, Direction: protocol.ForwardTapDirection_ToTarget, DroppedBytes: 900})
	if got := NewTapRenderer(TapHex).Render(rec); len(got) != 1 || got[0] != tapHeader("flow 3", "gap", tapTimestamp(rec.UnixMs), "900B missed ->") {
		t.Fatalf("datagram gap = %q", got)
	}
	if got := NewTapRenderer(TapRaw).Render(rec); len(got) != 0 {
		t.Fatalf("raw rendered a gap: %q", got)
	}
}

func TestTapCloseReasons(t *testing.T) {
	closed := func(r protocol.PortForwardCloseReason, detail string) *protocol.ForwardTapRecord {
		rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_ForwardClosed, UnixMs: 1756000000000}
		fc := protocol.ForwardTapForwardClosed{Reason: r}
		fc.SetDetail([]byte(detail))
		rec.SetForwardClosed(fc)
		return rec
	}
	if got := NewTapRenderer(TapHex).Render(closed(protocol.PortForwardCloseReason_EndpointFailed, "runner: boom"))[0]; !strings.HasSuffix(got, "closed (endpoint failed: runner: boom)") {
		t.Fatalf("endpoint_failed = %q", got)
	}
	if got := NewTapRenderer(TapHex).Render(closed(protocol.PortForwardCloseReason_RunnerGone, ""))[0]; !strings.HasSuffix(got, "closed (runner gone)") {
		t.Fatalf("runner_gone = %q", got)
	}
	var j map[string]any
	_ = json.Unmarshal([]byte(NewTapRenderer(TapJSON).Render(closed(protocol.PortForwardCloseReason_EndpointFailed, "runner: boom"))[0]), &j)
	if j["reason"] != "endpoint failed" || j["detail"] != "runner: boom" {
		t.Fatalf("json = %v", j)
	}
}
