package objproto

import (
	"context"
	"net/netip"
	"time"
)

type ChanWithTimeout[T any] struct {
	C <-chan T
}

func (c *ChanWithTimeout[T]) WaitWithTimeout(ctx context.Context, timeout time.Duration) (T, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case v, ok := <-c.C:
		if !ok {
			var zero T
			return zero, ErrChannelClosed
		}
		return v, nil
	case <-timeoutCtx.Done():
		var zero T
		return zero, ErrTimeout
	}
}

type Session interface {
	SendHandshake(cid ConnectionID, priv []byte, hs *Handshake) (*ChanWithTimeout[Connection], error)
	SendProbe(cid ConnectionID, macAddr [6]byte, ipAddr netip.AddrPort) error
	GetNewActiveSessionChannel() <-chan Connection
	WaitNewActiveSession(timeout time.Duration) (Connection, error)
	GetProbeInfoChannel() <-chan *ProbeInfo
	WaitProbeInfo(timeout time.Duration) (*ProbeInfo, error)
	GetConnection(cid ConnectionID) (Connection, bool)
	ListHandshakes() []HandshakeInfo
	DeleteHandshakeBefore(limit time.Time) []HandshakeInfo
	ListActiveConnections() []Connection
	DeleteInactiveSessionsBefore(limit time.Time) []Connection
	ListProxies() []ProxyInfo
	DeleteProxyBefore(limit time.Time) []ProxyInfo
	SessionMode() SessionMode
	SetProxy(owned, allocate ConnectionID) error
	DeleteProxy(peer ConnectionID) error
}

func AutoGarbageCollect(s Session, interval time.Duration, handshakeTimeout, sessionTimeout, proxyTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		now := time.Now()
		s.DeleteHandshakeBefore(now.Add(-handshakeTimeout))
		s.DeleteInactiveSessionsBefore(now.Add(-sessionTimeout))
		s.DeleteProxyBefore(now.Add(-proxyTimeout))
	}
}

func AutoRespondProbes(s Session, macAddr [6]byte, ipAddr netip.AddrPort) {
	probes := s.GetProbeInfoChannel()
	for probe := range probes {
		s.SendProbe(probe.Sender, macAddr, ipAddr)
	}
}

type RawSession interface {
	Session
	GetSenderChannel() <-chan *PacketData
	Receive(transport string, from netip.AddrPort, data []byte) error
	CannotSend(*PacketData)
}

type PacketNumber = uint64

type Connection interface {
	SetName(name string)
	Name() string
	ConnectionID() ConnectionID
	ConsumePacketNumber() PacketNumber
	SendMessageWithPacketNumber(obj []byte, pn PacketNumber) (sentSize int, _ PacketNumber, _ error)
	SendMessage(obj []byte) (sentSize int, _ PacketNumber, _ error)
	ReceiveMessage() (*Message, error)
	ReceiveMessageTimeout(ctx context.Context, timeout time.Duration) (*Message, error)
	ReceiveMessageContext(ctx context.Context) (*Message, error)
	GetTranscript() []byte
	ConnectedAt() time.Time
	LastTime() time.Time
	Close() error
	IsActive() bool
	Done() <-chan struct{}
	// for proxy connections
	// proxy is established by upper layer negotiation and using Session.SetProxy.
	// Then rehandshake is performed to switch the connection to proxied peer.
	// Connection returned by RehandshakeForProxy is as same as the original connection but proxied
	RehandshakeForProxy(priv []byte, hs *Handshake) (*ChanWithTimeout[Connection], error)
	IsProxied() bool
}
