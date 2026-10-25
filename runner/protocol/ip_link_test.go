package protocol

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
)

func TestCarriesDatagrams(t *testing.T) {
	for p, want := range map[ForwardProtocol]bool{
		ForwardProtocol_Tcp: false,
		ForwardProtocol_Udp: true,
		ForwardProtocol_Ip:  true,
	} {
		if got := p.CarriesDatagrams(); got != want {
			t.Errorf("%v.CarriesDatagrams() = %v, want %v", p, got, want)
		}
	}
}

func TestIpLinkConfigValidate(t *testing.T) {
	cases := []struct {
		client, runner string
		want           string // "" = valid
	}{
		{"10.200.0.1/30", "10.200.0.2", ""},
		{"10.0.0.0/31", "10.0.0.1", ""}, // RFC 3021: a /31 has no network or broadcast address
		{"10.0.0.1/1", "10.0.0.2", ""},
		{"10.200.0.1/32", "10.200.0.2", "prefix /32 is outside 1-31"},
		{"10.200.0.1/0", "10.200.0.2", "prefix /0 is outside 1-31"},
		{"10.200.0.1/30", "10.200.0.1", "the two addresses are the same"},
		{"10.200.0.1/30", "10.200.0.9", "10.200.0.9 is not inside 10.200.0.0/30"},
		{"10.200.0.0/30", "10.200.0.2", "10.200.0.0 is the network address of 10.200.0.0/30"},
		{"10.200.0.1/30", "10.200.0.3", "10.200.0.3 is the broadcast address of 10.200.0.0/30"},
		{"fd00::1/64", "fd00::2", "ipv6 link addresses are not implemented"},
	}
	for _, c := range cases {
		_, err := NewIpLinkConfig(netip.MustParsePrefix(c.client), netip.MustParseAddr(c.runner))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s / %s: unexpected error %v", c.client, c.runner, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s / %s: error = %v, want it to contain %q", c.client, c.runner, err, c.want)
		}
		if err != nil && c.want != "ipv6 link addresses are not implemented" && !strings.HasPrefix(err.Error(), "ip link: ") {
			t.Errorf("%s / %s: %q does not start with \"ip link: \"", c.client, c.runner, err)
		}
	}
	if _, err := NewIpLinkConfig(netip.MustParsePrefix("fd00::1/64"), netip.MustParseAddr("fd00::2")); err == nil ||
		err.Error() != "ipv6 link addresses are not implemented" {
		t.Errorf("ipv6 error = %v, want exactly the spec's text", err)
	}

	odd := IpLinkConfig{AddrLen: 5, ClientAddr: make([]byte, 5), RunnerAddr: make([]byte, 5), PrefixLen: 30, Mtu: IpLinkMTU}
	if err := odd.Validate(); err == nil || !strings.Contains(err.Error(), "address length 5") {
		t.Errorf("addr_len 5: error = %v", err)
	}
	cfg, err := NewIpLinkConfig(netip.MustParsePrefix("10.200.0.1/30"), netip.MustParseAddr("10.200.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mtu = 1500
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mtu 1500 is not 1100") {
		t.Errorf("mtu 1500: error = %v", err)
	}
}

func TestIpLinkConfigPrefixes(t *testing.T) {
	cfg, err := NewIpLinkConfig(netip.MustParsePrefix("10.200.0.1/30"), netip.MustParseAddr("10.200.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ClientPrefix().String(); got != "10.200.0.1/30" {
		t.Errorf("ClientPrefix = %s", got)
	}
	if got := cfg.RunnerPrefix().String(); got != "10.200.0.2/30" {
		t.Errorf("RunnerPrefix = %s", got)
	}
	if cfg.Mtu != 1100 {
		t.Errorf("Mtu = %d, want 1100", cfg.Mtu)
	}
	var zero IpLinkConfig
	if zero.ClientPrefix().IsValid() || zero.RunnerPrefix().IsValid() {
		t.Error("a zero config must give the zero Prefix")
	}
}

func TestIpLinkConfigRoundTrip(t *testing.T) {
	cfg, err := NewIpLinkConfig(netip.MustParsePrefix("10.200.0.1/30"), netip.MustParseAddr("10.200.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	same := func(t *testing.T, got *IpLinkConfig) {
		t.Helper()
		if got == nil {
			t.Fatal("Link() = nil after decode")
		}
		if got.AddrLen != 4 || !bytes.Equal(got.ClientAddr, cfg.ClientAddr) ||
			!bytes.Equal(got.RunnerAddr, cfg.RunnerAddr) || got.PrefixLen != 30 || got.Mtu != 1100 {
			t.Fatalf("Link() = %+v, want %+v", got, cfg)
		}
	}

	t.Run("RegisterPortForwardRequest", func(t *testing.T) {
		in := RegisterPortForwardRequest{Direction: PortForwardDirection_Link, Protocol: ForwardProtocol_Ip}
		if !in.SetLink(cfg) {
			t.Fatal("SetLink refused on an ip request")
		}
		var out RegisterPortForwardRequest
		if err := out.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		same(t, out.Link())

		udp := RegisterPortForwardRequest{Protocol: ForwardProtocol_Udp}
		before := udp.MustAppend(nil)
		if udp.SetLink(cfg) {
			t.Fatal("SetLink accepted on a udp request")
		}
		if !bytes.Equal(before, udp.MustAppend(nil)) {
			t.Fatal("a refused SetLink changed the encoding")
		}
	})
	t.Run("RunnerOpenPortForwardRequest", func(t *testing.T) {
		in := RunnerOpenPortForwardRequest{Direction: PortForwardDirection_Link, Protocol: ForwardProtocol_Ip, ForwardId: 9}
		if !in.SetLink(cfg) {
			t.Fatal("SetLink refused")
		}
		var out RunnerOpenPortForwardRequest
		if err := out.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		same(t, out.Link())
	})
	t.Run("PortForwardInfo", func(t *testing.T) {
		in := PortForwardInfo{Direction: PortForwardDirection_Link, Protocol: ForwardProtocol_Ip}
		if !in.SetLink(cfg) {
			t.Fatal("SetLink refused")
		}
		var out PortForwardInfo
		if err := out.DecodeExact(in.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		same(t, out.Link())
	})
}

func TestForwardTapDatagramRoundTrip(t *testing.T) {
	rec := ForwardTapRecord{Kind: ForwardTapRecordKind_Datagram, UnixMs: 7}
	d := ForwardTapDatagram{FlowId: 3, Direction: ForwardTapDirection_FromTarget, Seq: 41,
		Fate: ForwardTapDatagramFate_DroppedCongestion, TruncatedBytes: 2}
	d.SetData([]byte("abc"))
	rec.SetDatagram(d)
	var out ForwardTapRecord
	if err := out.DecodeExact(rec.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	got := out.Datagram()
	if got == nil || got.FlowId != 3 || got.Seq != 41 || got.Fate != ForwardTapDatagramFate_DroppedCongestion ||
		got.TruncatedBytes != 2 || string(got.Data) != "abc" || got.Direction != ForwardTapDirection_FromTarget {
		t.Fatalf("datagram = %+v", got)
	}

	gap := ForwardTapRecord{Kind: ForwardTapRecordKind_DatagramGap}
	gap.SetDatagramGap(ForwardTapDatagramGap{FlowId: 3, Direction: ForwardTapDirection_ToTarget, DroppedBytes: 900})
	var gout ForwardTapRecord
	if err := gout.DecodeExact(gap.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if g := gout.DatagramGap(); g == nil || g.FlowId != 3 || g.DroppedBytes != 900 {
		t.Fatalf("gap = %+v", g)
	}
}
