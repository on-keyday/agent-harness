package tui

import "testing"

func TestRawConnectModal_OpenCloseAndSpec(t *testing.T) {
	m := NewRawConnectModal()
	if m.IsOpen() {
		t.Fatal("zero modal must be closed")
	}
	m.Open("4a1f0000000000000000000000000000")
	if !m.IsOpen() || m.TaskID() == "" {
		t.Fatal("Open must record the task and open")
	}
	m.SetSpec("127.0.0.1:6379")
	host, port, err := m.Target()
	if err != nil || host != "127.0.0.1" || port != 6379 {
		t.Fatalf("Target() = %s:%d err=%v", host, port, err)
	}
	m.SetSpec("garbage")
	if _, _, err := m.Target(); err == nil {
		t.Fatal("Target() must reject a spec that is not host:port")
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("Close must close")
	}
}

func TestRawConnectModal_RingCap(t *testing.T) {
	m := NewRawConnectModal()
	m.Open("4a1f0000000000000000000000000000")
	big := make([]byte, rawTUIRingBytes+1024)
	for i := range big {
		big[i] = 'x'
	}
	m.AppendOutput(big)
	if got := len(m.Output()); got > rawTUIRingBytes {
		t.Fatalf("output ring = %d bytes, want <= %d", got, rawTUIRingBytes)
	}
}
