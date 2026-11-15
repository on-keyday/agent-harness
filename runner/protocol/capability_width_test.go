package protocol

import (
	"math/bits"
	"reflect"
	"testing"
)

// capabilityBitsBeforeWidening is how many Capability bits may be used while
// the type is u32. 30 are taken (pillar S's `schedule` was the 30th). The next
// bit must not just take the 31st slot:
//
//   - The WebUI tests caps with JavaScript's `&`, which works on signed 32-bit
//     integers, so a value with bit 31 set reads as negative and every
//     `(bits & c.bit) === c.bit` check against it goes wrong.
//   - At 32 the type is full and a widening becomes urgent instead of planned.
//
// Widening to u64 was costed and is cheap: the fields that carry a Capability
// are all client<->server formats (no runner-decoded one), and the WAL stores
// caps as JSON numbers. Do it first, as its own change, then raise this bound.
// Never merge two gates into one bit to stay under it.
const capabilityBitsBeforeWidening = 30

func TestCapabilityStaysWithinTheU32Budget(t *testing.T) {
	if reflect.TypeOf(Capability(0)).Kind() != reflect.Uint32 {
		return // widened; this budget no longer applies
	}
	if used := bits.Len32(uint32(Capability_All)); used > capabilityBitsBeforeWidening {
		t.Fatalf("Capability_All uses %d bits of a u32 (budget %d): widen Capability to u64 "+
			"in message.bgn first, as its own change (see the comment on "+
			"capabilityBitsBeforeWidening)", used, capabilityBitsBeforeWidening)
	}
}
