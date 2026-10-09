package protocol

import "testing"

// Every arm of ExecTapRecord round-trips, and the records are self-delimiting:
// two concatenated decode as two, which is what the tap stream relies on.
func TestExecTapRecordArmsRoundTripConcatenated(t *testing.T) {
	d := ExecTapData{Channel: ExecTapChannel_Stdin, StreamOffset: 7, TruncatedBytes: 2}
	d.SetData([]byte{0x00, 0xff, 'x'})
	data := ExecTapRecord{Kind: ExecTapRecordKind_Data, UnixMs: 1}
	data.SetData(d)
	gap := ExecTapRecord{Kind: ExecTapRecordKind_Gap, UnixMs: 2}
	gap.SetGap(ExecTapGap{Channel: ExecTapChannel_Stderr, DroppedBytes: 9})
	eof := ExecTapRecord{Kind: ExecTapRecordKind_Eof, UnixMs: 3}
	eof.SetEof(ExecTapEof{Channel: ExecTapChannel_Stdout})
	end := ExecTapRecord{Kind: ExecTapRecordKind_ExecEnded, UnixMs: 4}
	end.SetExecEnded(ExecTapExecEnded{Kind: ExecEventKind_Exited, ExitCode: 3})

	var buf []byte
	for _, r := range []*ExecTapRecord{&data, &gap, &eof, &end} {
		buf = r.MustAppend(buf)
	}
	var got []*ExecTapRecord
	for len(buf) > 0 {
		r := &ExecTapRecord{}
		rest, err := r.Decode(buf)
		if err != nil {
			t.Fatalf("decode after %d records: %v", len(got), err)
		}
		got = append(got, r)
		buf = rest
	}
	if len(got) != 4 {
		t.Fatalf("decoded %d records, want 4", len(got))
	}
	if g := got[0].Data(); g == nil || g.StreamOffset != 7 || string(g.Data) != "\x00\xffx" || g.TruncatedBytes != 2 {
		t.Fatalf("data arm: %+v", g)
	}
	if g := got[1].Gap(); g == nil || g.Channel != ExecTapChannel_Stderr || g.DroppedBytes != 9 {
		t.Fatalf("gap arm: %+v", g)
	}
	if g := got[2].Eof(); g == nil || g.Channel != ExecTapChannel_Stdout {
		t.Fatalf("eof arm: %+v", g)
	}
	if g := got[3].ExecEnded(); g == nil || g.Kind != ExecEventKind_Exited || g.ExitCode != 3 {
		t.Fatalf("exec_ended arm: %+v", g)
	}
}

// The listing row carries the counters end to end.
func TestExecRunInfoCountersRoundTrip(t *testing.T) {
	in := ExecRunInfo{ExecId: 4, StdinBytes: 1, StdoutBytes: 2, StderrBytes: 3, LastActivityUnixMs: 5, Taps: 6}
	var out ExecRunInfo
	if err := out.DecodeExact(in.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if out.StdinBytes != 1 || out.StdoutBytes != 2 || out.StderrBytes != 3 || out.LastActivityUnixMs != 5 || out.Taps != 6 {
		t.Fatalf("round trip: %+v", out)
	}
}
