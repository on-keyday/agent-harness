package protocol

import (
	"net/netip"

	"github.com/on-keyday/objtrsf/objproto"
)

// ConnID ↔ objproto.ConnectionID, the runtime type everything actually dials.
//
// Deliberately a method plus a constructor rather than a matching pair of free
// functions: the existing pair (RunnerIDToConnID / ConnIDToRunnerID) already
// uses "ConnID" in its names to mean objproto's type, so a free function named
// ConnIDToObjproto would sit next to ConnIDToRunnerID with a different input
// type — the same conflation this split exists to remove, in miniature. Those
// two are transitional: the address callers move here, and the identity callers
// stop needing a converter at all once RunnerID is opaque.

// ToObjproto converts a wire ConnID to the dialable runtime form. A zero or
// unset ip_addr_len leaves the address invalid rather than panicking, so a
// missing ConnID stringifies as clearly malformed instead of taking the process
// down; callers that must distinguish check TransportLen != 0 (an absent ConnID
// carries no transport).
func (c ConnID) ToObjproto() objproto.ConnectionID {
	var ip netip.Addr
	switch c.IpAddrLen {
	case 4:
		ip = netip.AddrFrom4([4]byte(c.IpAddr))
	case 16:
		ip = netip.AddrFrom16([16]byte(c.IpAddr))
	}
	return objproto.ConnectionID{
		Transport: string(c.Transport),
		Addr:      netip.AddrPortFrom(ip, c.Port),
		ID:        c.UniqueNumber,
	}
}

// ConnIDFromObjproto is the inverse.
func ConnIDFromObjproto(cid objproto.ConnectionID) ConnID {
	var c ConnID
	c.SetTransport([]byte(cid.Transport))
	c.SetIpAddr(cid.Addr.Addr().AsSlice())
	c.Port = uint16(cid.Addr.Port())
	c.UniqueNumber = cid.ID
	return c
}
