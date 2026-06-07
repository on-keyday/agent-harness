package protocol

import (
	"net/netip"

	"github.com/on-keyday/objtrsf/objproto"
)

// runnerIDToConnID converts protocol.RunnerID → objproto.ConnectionID without
// going through wire encode/decode (so a zero RunnerID does not panic on the
// IpAddrLen invariant).
func RunnerIDToConnID(rid RunnerID) objproto.ConnectionID {
	var ip netip.Addr
	switch rid.IpAddrLen {
	case 4:
		ip = netip.AddrFrom4([4]byte(rid.IpAddr))
	case 16:
		ip = netip.AddrFrom16([16]byte(rid.IpAddr))
	default:
		// zero-value or unset; leave ip invalid — caller stringification
		// will produce a clearly-malformed CID.
	}
	return objproto.ConnectionID{
		Transport: string(rid.Transport),
		Addr:      netip.AddrPortFrom(ip, rid.Port),
		ID:        rid.UniqueNumber,
	}
}

func ConnIDToRunnerID(cid objproto.ConnectionID) RunnerID {
	var rid RunnerID
	rid.SetTransport([]byte(cid.Transport))
	ip := cid.Addr.Addr().AsSlice()
	rid.SetIpAddr(ip)
	rid.Port = uint16(cid.Addr.Port())
	rid.UniqueNumber = cid.ID
	return rid
}
