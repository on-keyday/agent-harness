package server

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// decodeWALSelector decodes a persisted RunnerSelector, migrating the one arm
// whose payload changed shape under it.
//
// The WAL stores the selector as raw wire bytes (walEventJSON.SelectorB64), so
// a schema change to RunnerSelector reaches back into every record ever
// written. by_runner_id changed exactly that way when RunnerID became an opaque
// 16-byte identity: the arm used to carry the address-shaped
// transport/ip_addr/port/unique_number tuple, and the new decoder reads its 13
// bytes as a short RunnerID::Id and fails with
//
//	not enough data to read for field "RunnerID::Id"
//
// The old payload is not corrupt and it is not lost data — it is a ConnID.
// Byte-identical by construction, which the format comment in message.bgn says
// is deliberate, and a connection id is what that field actually held: an
// operator who typed `--runner ws:host:port-7` was naming a CONNECTION then and
// still is. So the migration re-types the arm rather than discarding it, under
// the name that now says which meaning it has.
//
// The two shapes cannot be confused. A new payload is exactly 16 bytes. An old
// one is 1+len(transport)+1+len(ip_addr)+2+2 over transport in {udp, ws, wss}
// and ip_addr in {0, 4, 16} bytes, so it is one of 8, 9, 12, 13, 24 or 25 —
// never 16. The current decoder is therefore tried first and only its failure
// reaches the migration, so a record written by this binary can never be read
// back through the legacy path.
//
// migrated reports whether the legacy path was taken, so the caller can say so
// once for the whole file instead of once per record on every boot.
func decodeWALSelector(wire []byte) (sel protocol.RunnerSelector, migrated bool, err error) {
	if err := sel.DecodeExact(wire); err == nil {
		return sel, false, nil
	} else {
		// Held for the return: if the legacy path does not apply, the error the
		// caller wants is the one from the CURRENT schema, not a second failure
		// explaining that 13 bytes are not an address either.
		currentErr := err

		if len(wire) == 0 || protocol.RunnerSelectorKind(wire[0]) != protocol.RunnerSelectorKind_ByRunnerId {
			return protocol.RunnerSelector{}, false, currentErr
		}
		var cid protocol.ConnID
		// Copy, not alias: wire is the base64 scratch buffer of one JSON line
		// and the ConnID's transport slice outlives it.
		if cerr := cid.DecodeExactCopy(wire[1:]); cerr != nil {
			return protocol.RunnerSelector{}, false, currentErr
		}
		var mig protocol.RunnerSelector
		mig.Kind = protocol.RunnerSelectorKind_ByConnId
		if !mig.SetConnId(cid) {
			return protocol.RunnerSelector{}, false, fmt.Errorf("migrating a legacy by_runner_id selector: SetConnId refused the arm (%w)", currentErr)
		}
		return mig, true, nil
	}
}
