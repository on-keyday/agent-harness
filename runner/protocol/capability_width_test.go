package protocol

import (
	"math/bits"
	"reflect"
	"testing"
)

// capabilityBitsExactInJS is how many Capability bits the WebUI can carry. The
// wasm bridge passes a mask as a JavaScript Number (a float64), which holds an
// integer exactly only up to 2^53, and the page's capHas / capOr / capXor
// (webui/static/task_models.js) compute on BigInts made from those Numbers.
// A 54th bit needs the masks to cross the bridge as strings or BigInts first.
// Never merge two gates into one bit to stay under it.
const capabilityBitsExactInJS = 53

func TestCapabilityIsU64(t *testing.T) {
	if k := reflect.TypeOf(Capability(0)).Kind(); k != reflect.Uint64 {
		t.Fatalf("Capability is %v, want uint64 (docs/superpowers/specs/2026-11-16-capability-u64-design.md)", k)
	}
}

func TestCapabilityStaysExactInTheWebUI(t *testing.T) {
	if used := bits.Len64(uint64(Capability_All)); used > capabilityBitsExactInJS {
		t.Fatalf("Capability_All uses %d bits; the WebUI carries masks as Numbers, exact to %d "+
			"(see capabilityBitsExactInJS)", used, capabilityBitsExactInJS)
	}
}
