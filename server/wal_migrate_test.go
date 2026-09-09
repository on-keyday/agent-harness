package server

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// legacyByRunnerIDSelector builds the wire bytes a pre-identity server wrote
// for `--runner udp:192.0.2.1:1234-7`, by hand: the kind byte, then the
// address-shaped RunnerID that arm used to carry —
// transport_len/transport/ip_addr_len/ip_addr/port/unique_number, 13 bytes.
// Hand-built because the encoder that produced it no longer exists in the
// tree, which is the whole reason this class of record could not be read back.
func legacyByRunnerIDSelector() []byte {
	return []byte{
		byte(protocol.RunnerSelectorKind_ByRunnerId),
		0x03, 'u', 'd', 'p', // transport_len + transport
		0x04, 192, 0, 2, 1, // ip_addr_len + ip_addr
		0x04, 0xD2, // port = 1234
		0x00, 0x07, // unique_number = 7
	}
}

// The incident, end to end.
//
// Landing the opaque RunnerID made every legacy `--runner`-pinned record
// undecodable ("not enough data to read for field \"RunnerID::Id\""), and
// because ReadWAL's failure was file-wide, ONE such record took the entire task
// history with it: the server booted with an empty store and `restore` reported
// nothing to put back, with every byte still on disk.
//
// Both halves are pinned here because either alone would have prevented it, and
// each guards a different future change: the migration keeps THIS record
// readable, the locality keeps the next unreadable record cheap.
func TestLegacyByRunnerIdSelectorMigratesToByConnId(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(legacyByRunnerIDSelector())
	path := filepath.Join(t.TempDir(), "events.log")
	body := `{"type":"task_created","task_id":"aa","repo_path":"/r","prompt":"before","ts":1}
{"type":"task_created","task_id":"bb","repo_path":"/r","prompt":"pinned","selector_b64":"` + b64 + `","ts":2}
{"type":"task_created","task_id":"cc","repo_path":"/r","prompt":"after","ts":3}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	events, report, err := ReadWAL(path)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	if len(report.Defects) != 0 {
		t.Fatalf("a migratable record was skipped instead: %+v", report.Defects)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want all 3 — one legacy selector must not cost the history", len(events))
	}
	if report.Migrated != 1 {
		t.Errorf("report.Migrated = %d, want 1", report.Migrated)
	}

	// Re-typed, not discarded: the bytes said which connection, and they still do.
	pinned := events[1]
	if !pinned.SelectorMigrated {
		t.Error("the migrated record does not report itself as migrated")
	}
	if pinned.Selector.Kind != protocol.RunnerSelectorKind_ByConnId {
		t.Fatalf("selector kind = %v, want ByConnId", pinned.Selector.Kind)
	}
	cid := pinned.Selector.ConnId()
	if cid == nil {
		t.Fatal("ByConnId arm carries no payload")
	}
	if got := string(cid.Transport); got != "udp" {
		t.Errorf("transport = %q, want udp", got)
	}
	if got := []byte(cid.IpAddr); len(got) != 4 || got[0] != 192 || got[1] != 0 || got[2] != 2 || got[3] != 1 {
		t.Errorf("ip_addr = %v, want 192.0.2.1", got)
	}
	if cid.Port != 1234 || cid.UniqueNumber != 7 {
		t.Errorf("port/unique = %d/%d, want 1234/7", cid.Port, cid.UniqueNumber)
	}
}

// A current-schema by_runner_id must not be re-read through the legacy path.
// The two shapes are distinguishable by length — 16 bytes exactly for an
// identity, one of 8/9/12/13/24/25 for an address over transport in
// {udp, ws, wss} — so this is the assertion that the discriminator holds, and
// the one that would go red if a transport name six or ten bytes long ever
// appeared.
func TestCurrentByRunnerIdSelectorIsNotMigrated(t *testing.T) {
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByRunnerId}
	id := protocol.RunnerID{Id: [16]byte{0x3f, 0x22, 0xc0, 0xa9, 9, 9, 9, 9, 1, 2, 3, 4, 5, 6, 7, 8}}
	if !sel.SetRunnerId(id) {
		t.Fatal("SetRunnerId refused the arm")
	}
	wire, err := sel.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != 17 {
		t.Fatalf("current by_runner_id is %d bytes, not 1+16 — the length discriminator is void", len(wire))
	}
	got, migrated, derr := decodeWALSelector(wire)
	if derr != nil {
		t.Fatalf("decodeWALSelector: %v", derr)
	}
	if migrated {
		t.Error("a current-schema record took the legacy path")
	}
	if got.Kind != protocol.RunnerSelectorKind_ByRunnerId || got.RunnerId().Hex() != id.Hex() {
		t.Errorf("round trip lost the identity: kind=%v id=%s", got.Kind, got.RunnerId().Hex())
	}
}

// A selector that is neither shape is a defect, and the error the operator sees
// must be the CURRENT schema's — not a second failure explaining that 5 bytes
// are not an address either.
func TestUndecodableSelectorReportsTheCurrentSchemaError(t *testing.T) {
	wire := []byte{byte(protocol.RunnerSelectorKind_ByRunnerId), 1, 2, 3, 4}
	if _, migrated, err := decodeWALSelector(wire); err == nil || migrated {
		t.Fatalf("garbage decoded: migrated=%v err=%v", migrated, err)
	} else if want := "RunnerID::Id"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the current schema's field %q", err, want)
	}
}

// The WAL persists exactly ONE generated wire format, and this is the assertion
// that keeps that number known.
//
// A field whose type comes from the protocol package is stored as raw wire
// bytes, so its schema is frozen into every record ever written and a later
// change to it reaches back through the whole file. Selector is that field, it
// changed, and nothing anywhere said it would need a migration — the fact was
// true but unstated, which is why the consequence was not predicted. Embedding
// a second such type is a decision to take on the same obligation, so it should
// cost a red test and a look at decodeWALSelector rather than pass unremarked.
func TestOnlySelectorEmbedsAWireFormatInTheWAL(t *testing.T) {
	const protocolPkg = "github.com/on-keyday/agent-harness/runner/protocol"
	want := map[string]bool{"Selector": true}
	rt := reflect.TypeOf(WALEvent{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.PkgPath() != protocolPkg {
			continue
		}
		if !want[f.Name] {
			t.Errorf("WALEvent.%s is a %s wire format persisted in the WAL, and no "+
				"legacy-decode path covers it. Its schema is now frozen into every "+
				"record written from here on: either store a decomposed form, or give "+
				"it a migration next to decodeWALSelector and add it to this list.",
				f.Name, protocolPkg)
			continue
		}
		delete(want, f.Name)
	}
	for name := range want {
		t.Errorf("WALEvent.%s no longer embeds a wire format — drop it from this "+
			"list, and from decodeWALSelector if nothing else needs it", name)
	}
}
