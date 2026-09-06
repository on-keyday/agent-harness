package protocol

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/trsf"
)

// setEveryNumber fills every numeric field of v — walking nested structs — with
// a distinct value, and records what each one was. Durations get whole
// microseconds so the value survives the .Microseconds() the projection applies.
//
// Reflection rather than a hand-written fixture, because the fixture is the
// thing that rots: the previous version of this test listed the fields by hand,
// so a field added to InternalState and forgotten in BOTH places would have left
// it green. Slices (SentPackets) and non-numeric fields are skipped — the
// projection does not carry them and says why.
func setEveryNumber(t *testing.T, v reflect.Value, next *uint64, want map[uint64]string, path string) {
	t.Helper()
	durType := reflect.TypeOf(time.Duration(0))
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		name := path + v.Type().Field(i).Name
		switch {
		case f.Type() == durType:
			*next++
			f.SetInt(int64(time.Duration(*next) * time.Microsecond))
			want[*next] = name
		case f.Kind() == reflect.Struct:
			setEveryNumber(t, f, next, want, name+".")
		case f.CanInt():
			*next++
			f.SetInt(int64(*next))
			want[*next] = name
		case f.CanUint():
			*next++
			f.SetUint(*next)
			want[*next] = name
		}
	}
}

// Every number InternalState reports must reach the wire.
//
// No map of field names to keys is needed: each field gets a unique value, and
// every one of those values must come back among the counters. Add a field to
// InternalState, forget it in TrsfRowFrom, and this names the field.
func TestTrsfRowFromCarriesEveryNumberInInternalState(t *testing.T) {
	st := &trsf.InternalState{}
	want := map[uint64]string{}
	var next uint64 = 1000
	setEveryNumber(t, reflect.ValueOf(st).Elem(), &next, want, "")

	row := TrsfRowFrom(st)
	got := map[uint64]bool{}
	for _, c := range row.Counters {
		got[c.Value] = true
	}
	for v, name := range want {
		if !got[v] {
			t.Errorf("InternalState.%s (value %d) reaches no counter: TrsfRowFrom does not carry it",
				name, v)
		}
	}
	if int(row.CounterCount) != len(row.Counters) {
		t.Errorf("CounterCount = %d against %d counters", row.CounterCount, len(row.Counters))
	}
}

// Absent and zero are different answers, and the list is the only shape that
// can say so. A runner older than a counter omits it; rendering that omission
// as 0 would report a measurement nobody took.
func TestCounterSeparatesAbsentFromZero(t *testing.T) {
	row := TrsfRowFrom(&trsf.InternalState{}) // all zero, MinRTT unmeasured
	if v, ok := row.Counter(TrsfCounterKey_Cwnd); !ok || v != 0 {
		t.Errorf("cwnd = (%d, %v), want (0, true): a measured zero is present", v, ok)
	}
	if v, ok := row.Counter(TrsfCounterKey_MinRttUs); ok {
		t.Errorf("min_rtt_us = (%d, true) before any ACK, want absent", v)
	}
	if got := row.CounterOr(TrsfCounterKey_MinRttUs, 42); got != 42 {
		t.Errorf("CounterOr on an absent key = %d, want the default 42", got)
	}
	st := &trsf.InternalState{MinRTT: 3 * time.Millisecond}
	measured := TrsfRowFrom(st)
	if v, ok := measured.Counter(TrsfCounterKey_MinRttUs); !ok || v != 3000 {
		t.Errorf("min_rtt_us = (%d, %v), want (3000, true) once measured", v, ok)
	}
}

// The projection stays in ONE place. Before this, `server` and `runner` each
// built the row by hand from the same InternalState — so a field could land on
// one answerer and not the other, and the reader would see zeros from a runner
// with no way to tell that from an idle loop.
//
// A grep rather than a type check because that is the shape of the mistake:
// nothing stops a second literal compiling.
func TestNoHandWrittenTrsfRowsRemain(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	for _, pkg := range []string{"server", "runner", "cli", "tui"} {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				// A map or slice TYPE naming the struct is fine; a composite
				// literal that BUILDS one is the duplicate this guards.
				if !strings.Contains(line, "TrsfConnState{") {
					continue
				}
				if strings.Contains(line, "map[") || strings.Contains(line, "[]protocol.TrsfConnState{") {
					continue
				}
				t.Errorf("%s/%s:%d builds a TrsfConnState by hand: %s\n"+
					"use protocol.TrsfRowFrom so a new counter reaches every answerer",
					pkg, e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}
