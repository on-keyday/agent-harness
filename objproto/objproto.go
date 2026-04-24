package objproto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/objproto/packet"
)

type ConnectionID struct {
	Transport string
	Addr      netip.AddrPort
	ID        uint16
}

func (v *ConnectionID) UnmarshalText(text []byte) error {
	c, err := ParseConnectionID(string(text), 0)
	if err != nil {
		return err
	}
	*v = c
	return nil
}

func (v *ConnectionID) MarshalText() ([]byte, error) {
	if v == nil {
		return []byte{'*'}, nil
	}
	return []byte(v.String()), nil
}

func randomUint16() (uint16, error) {
	var b [2]byte
	_, err := rand.Read(b[:])
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

func NewConnectionID(transport string, addr netip.AddrPort, id uint16) ConnectionID {
	return ConnectionID{
		Transport: transport,
		Addr:      addr,
		ID:        id,
	}
}

func NewRandomConnectionID(transport string, addr netip.AddrPort) (ConnectionID, error) {
	id, err := randomUint16()
	if err != nil {
		return ConnectionID{}, err
	}
	return ConnectionID{
		Transport: transport,
		Addr:      addr,
		ID:        uint16(id),
	}, nil
}

func (c ConnectionID) String() string {
	return fmt.Sprintf("%s:%s-%d", c.Transport, c.Addr.String(), c.ID)
}

type ParseOption int

func (o ParseOption) Has(flag ParseOption) bool {
	return o&flag != 0
}

const (
	ParseOption_AllowRandomID ParseOption = 1 << iota
	ParseOption_ResolveAddr
)

func ParseConnectionID(s string, opt ParseOption) (ConnectionID, error) {
	var transport string
	var addrStr string
	// split by first ':' then split by '-'
	splited := strings.SplitN(s, ":", 2)
	if len(splited) != 2 {
		return ConnectionID{}, fmt.Errorf("invalid connection ID format")
	}
	nextSplited := strings.SplitN(splited[1], "-", 2)
	if len(nextSplited) != 2 {
		return ConnectionID{}, fmt.Errorf("invalid connection ID format")
	}
	transport = splited[0]
	addrStr = nextSplited[0]
	idStr := nextSplited[1]
	addr, err := netip.ParseAddrPort(addrStr)
	if err != nil {
		if !opt.Has(ParseOption_ResolveAddr) {
			return ConnectionID{}, err
		}
		addrPort := strings.SplitN(addrStr, ":", 2)
		if len(addrPort) != 2 {
			return ConnectionID{}, fmt.Errorf("invalid address format: %s", addrStr)
		}
		var port uint16
		portV, err := strconv.ParseUint(addrPort[1], 10, 16)
		if err != nil {
			return ConnectionID{}, fmt.Errorf("invalid port number: %s", addrPort[1])
		}
		port = uint16(portV)
		ipAddrs, err := net.LookupIP(addrPort[0])
		if err != nil || len(ipAddrs) == 0 {
			return ConnectionID{}, err
		}
		addrR, ok := netip.AddrFromSlice(ipAddrs[0])
		if !ok {
			return ConnectionID{}, fmt.Errorf("invalid IP address: %s", ipAddrs[0])
		}
		addr = netip.AddrPortFrom(addrR, port)
	}
	var idN uint16
	if idStr == "*" && opt.Has(ParseOption_AllowRandomID) {
		idN, err = randomUint16()
		if err != nil {
			return ConnectionID{}, err
		}
	} else {
		id, err := strconv.ParseUint(idStr, 10, 16)
		if err != nil {
			return ConnectionID{}, err
		}
		idN = uint16(id)
	}
	return ConnectionID{
		Transport: transport,
		Addr:      addr,
		ID:        idN,
	}, nil
}

// for test convenience
func MustParseConnectionID(s string) ConnectionID {
	cid, err := ParseConnectionID(s, 0)
	if err != nil {
		panic(err)
	}
	return cid
}

type handshakeInfo struct {
	ConnectionID ConnectionID
	KeyKind      packet.KeyKind
	PrivateKey   []byte
	LastTime     time.Time
	Transcript   []byte
	hsDone       chan Connection
	proxySession *ActiveSession
}

func (h *handshakeInfo) closeUnlocked() {
	close(h.hsDone)
	clear(h.PrivateKey)
	if h.proxySession != nil {
		h.proxySession.closeUnlocked()
	}
}

type nonceRange struct {
	start    uint64
	end      uint64 // inclusive
	inserted time.Time
}

type receiveNonceTracker struct {
	largestNonce  uint64
	lastNonceUsed time.Time
	unusedRange   []nonceRange
}

func (r *receiveNonceTracker) Reset() {
	r.largestNonce = 0
	r.lastNonceUsed = time.Time{}
	r.unusedRange = nil
}

func (r *receiveNonceTracker) InsertNonce(nonce uint64, now time.Time, dryRun bool) bool {
	if nonce > r.largestNonce {
		if dryRun {
			return true
		}
		// add new unused range if there is a gap
		if nonce > r.largestNonce+1 {
			r.unusedRange = append(r.unusedRange, nonceRange{
				start:    r.largestNonce + 1,
				end:      nonce - 1,
				inserted: now,
			})
		}
		r.largestNonce = nonce
	} else {
		exists := false
		// remove from unused ranges
		for i := 0; i < len(r.unusedRange); i++ {
			if r.unusedRange[i].start <= nonce && r.unusedRange[i].end >= nonce {
				exists = true
				if dryRun {
					return true
				}
				// found the range that contains the nonce
				if r.unusedRange[i].start == nonce && r.unusedRange[i].end == nonce {
					// remove the range
					r.unusedRange = append(r.unusedRange[:i], r.unusedRange[i+1:]...)
				} else if r.unusedRange[i].start == nonce {
					// shrink the range
					r.unusedRange[i].start++
				} else if r.unusedRange[i].end == nonce {
					// shrink the range
					r.unusedRange[i].end--
				} else {
					// split the range
					newRange := nonceRange{
						start:    nonce + 1,
						end:      r.unusedRange[i].end,
						inserted: now,
					}
					r.unusedRange[i].end = nonce - 1
					r.unusedRange = append(r.unusedRange[:i+1], append([]nonceRange{newRange}, r.unusedRange[i+1:]...)...)
				}
				break
			}
		}
		if !exists {
			return false
		}
		// merge adjacent ranges
		for i := 0; i < len(r.unusedRange)-1; i++ {
			if r.unusedRange[i].end+1 == r.unusedRange[i+1].start {
				// merge the two ranges
				r.unusedRange[i].end = r.unusedRange[i+1].end
				r.unusedRange[i].inserted = r.unusedRange[i+1].inserted
				r.unusedRange = append(r.unusedRange[:i+1], r.unusedRange[i+2:]...)
				i-- // recheck the merged range
			}
		}
	}
	return true
}

type ActiveSession struct {
	name              atomic.Value
	mu                sync.Mutex
	session           *session
	cid               ConnectionID
	connTime          time.Time
	lastTime          time.Time
	sessionSecret     cipher.AEAD
	selfIV            []byte
	peerIV            []byte
	selfHeaderProtect cipher.Block
	peerHeaderProtect cipher.Block
	msgs              *messageChannel
	sentCounter       atomic.Uint64
	recvTracker       receiveNonceTracker
	transcript        []byte
	closed            chan struct{}
	proxied           bool
}

func (a *ActiveSession) SetName(name string) {
	a.name.Store(name)
}

func (a *ActiveSession) Name() string {
	if v := a.name.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func (a *ActiveSession) ConnectionID() ConnectionID {
	return a.cid
}

func (a *ActiveSession) SendMessageWithPacketNumber(data []byte, pn PacketNumber) (int, PacketNumber, error) {
	return a.session.sendApplication(a.cid, data, a, &pn)
}

func (a *ActiveSession) SendMessage(data []byte) (int, PacketNumber, error) {
	return a.session.sendApplication(a.cid, data, a, nil)
}

func (a *ActiveSession) ReceiveMessage() (*Message, error) {
	return a.msgs.ReceiveMessage()
}

func (a *ActiveSession) ReceiveMessageTimeout(ctx context.Context, timeout time.Duration) (*Message, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, ErrTimeout)
	defer cancel()
	return a.msgs.ReceiveMessageContext(ctx)
}

func (a *ActiveSession) ReceiveMessageContext(ctx context.Context) (*Message, error) {
	return a.msgs.ReceiveMessageContext(ctx)
}

func (a *ActiveSession) GetTranscript() []byte {
	return a.transcript
}

func (a *ActiveSession) ConnectedAt() time.Time {
	return a.connTime
}

func (a *ActiveSession) LastTime() time.Time {
	return a.lastTime
}

func (a *ActiveSession) Close() error {
	select {
	case <-a.closed:
		return nil // already closed
	default:
	}
	return a.session.closeConnection(a)
}

func (a *ActiveSession) closeUnlocked() {
	close(a.closed)
	a.msgs.CloseChannel()
}

func (a *ActiveSession) IsActive() bool {
	select {
	case <-a.closed:
		return false
	default:
		return true
	}
}
func (a *ActiveSession) Done() <-chan struct{} {
	return a.closed
}

// for proxy connections
func (a *ActiveSession) RehandshakeForProxy(priv []byte, hs *Handshake) (*ChanWithTimeout[Connection], error) {
	return a.session.sendRehandshakeForProxy(a, priv, hs)
}

func (a *ActiveSession) IsProxied() bool {
	return a.proxied
}

func (a *ActiveSession) ConsumePacketNumber() PacketNumber {
	return PacketNumber(a.sentCounter.Add(1)) // from 1
}

var _ Connection = (*ActiveSession)(nil)

type PacketData struct {
	To   ConnectionID
	Kind packet.PacketKind
	Data []byte
}

type ProbeInfo struct {
	Sender     ConnectionID
	MacAddress [6]byte
	IpAddress  netip.AddrPort
}

type SessionMode string

const (
	SessionModeMutual SessionMode = "mutual"
	SessionModeServer SessionMode = "server"
	SessionModeClient SessionMode = "client"
)

type proxySetting struct {
	peer1    ConnectionID
	peer2    ConnectionID
	lastUsed time.Time
}

func (p *proxySetting) getPeer(cid ConnectionID) (ConnectionID, bool) {
	if subtle.ConstantTimeCompare([]byte(p.peer1.String()), []byte(cid.String())) == 1 {
		return p.peer2, true
	}
	if subtle.ConstantTimeCompare([]byte(p.peer2.String()), []byte(cid.String())) == 1 {
		return p.peer1, true
	}
	return ConnectionID{}, false
}

type session struct {
	sessionsLock   sync.RWMutex
	sentHandshake  map[ConnectionID]*handshakeInfo
	activeSessions map[ConnectionID]*ActiveSession
	probeInfo      chan *ProbeInfo
	pktQueue       chan *PacketData
	newActiveSess  chan Connection
	logger         *slog.Logger
	mode           SessionMode

	proxyLock     sync.Mutex
	proxySettings map[ConnectionID]*proxySetting
	cookieSecret  []byte
}

func NewSession(logger *slog.Logger, mode SessionMode) RawSession {
	if mode != SessionModeMutual && mode != SessionModeServer && mode != SessionModeClient {
		panic("invalid session mode")
	}
	return &session{
		sentHandshake:  make(map[ConnectionID]*handshakeInfo),
		activeSessions: make(map[ConnectionID]*ActiveSession),
		pktQueue:       make(chan *PacketData, 1024),
		newActiveSess:  make(chan Connection, 1024),
		probeInfo:      make(chan *ProbeInfo, 1024),
		logger:         logger,
		mode:           mode,
		proxySettings:  make(map[ConnectionID]*proxySetting),
	}
}

func (s *session) SetProxy(owned, allocate ConnectionID) error {
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	if _, exists := s.activeSessions[owned]; !exists {
		return fmt.Errorf("owned connection not found")
	}
	if _, exists := s.activeSessions[allocate]; exists {
		return fmt.Errorf("allocate connection already exists")
	}
	s.proxyLock.Lock()
	defer s.proxyLock.Unlock()
	setting := &proxySetting{
		peer1:    owned,
		peer2:    allocate,
		lastUsed: time.Now(),
	}
	s.proxySettings[owned] = setting
	s.proxySettings[allocate] = setting
	s.logger.Info("set proxy setting", "owned", owned.String(), "allocate", allocate.String())
	return nil
}

func (s *session) DeleteProxy(peer ConnectionID) error {
	s.proxyLock.Lock()
	defer s.proxyLock.Unlock()
	if setting, exists := s.proxySettings[peer]; exists {
		delete(s.proxySettings, setting.peer1)
		delete(s.proxySettings, setting.peer2)
		s.logger.Info("deleted proxy setting", "peer1", setting.peer1.String(), "peer2", setting.peer2.String())
		return nil
	}
	return fmt.Errorf("proxy setting not found for %s", peer.String())
}

func (s *session) SessionMode() SessionMode {
	return s.mode
}

func (s *session) deleteActiveSession(cid ConnectionID) {
	delete(s.activeSessions, cid)
}

func (s *session) deleteHandshake(cid ConnectionID) {
	delete(s.sentHandshake, cid)
}

func (s *session) closeConnection(a *ActiveSession) error {
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	if session, exists := s.activeSessions[a.cid]; exists && session == a {
		s.deleteActiveSession(a.cid)
		a.closeUnlocked()
		s.logger.Info("active session closed", "cid", a.cid.String())
	}
	return nil
}

func (s *session) mayCloseProxy(pkt *PacketData) bool {
	s.proxyLock.Lock()
	defer s.proxyLock.Unlock()
	setting, exists := s.proxySettings[pkt.To]
	if exists {
		delete(s.proxySettings, setting.peer1)
		delete(s.proxySettings, setting.peer2)
		return true
	}
	return false
}

func (s *session) closeCannotSend(pkt *PacketData) {
	if s.mayCloseProxy(pkt) {
		s.logger.Info("proxy session closed due to cannot send", "cid", pkt.To.String())
		return
	}
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	switch pkt.Kind {
	case packet.PacketKind_Handshake:
		if sent, exists := s.sentHandshake[pkt.To]; exists {
			sent.closeUnlocked()
			s.deleteHandshake(pkt.To)
			s.logger.Info("sent handshake removed due to cannot send", "cid", pkt.To.String())
		} else {
			s.logger.Warn("cannot send for unknown handshake", "cid", pkt.To.String())
		}
	case packet.PacketKind_HandshakeAck, packet.PacketKind_Application:
		if session, exists := s.activeSessions[pkt.To]; exists {
			s.deleteActiveSession(pkt.To)
			session.closeUnlocked()
			s.logger.Info("active session closed due to cannot send", "cid", pkt.To.String())
		} else {
			s.logger.Warn("cannot send for unknown active session", "cid", pkt.To.String())
		}
	case packet.PacketKind_Probe:
		// no action needed for probes
	default:
		s.logger.Warn("cannot send for unknown packet kind", "kind", pkt.Kind, "cid", pkt.To.String())
	}
}

func (s *session) GetSenderChannel() <-chan *PacketData {
	return s.pktQueue
}

func (s *session) sendPacket(cid ConnectionID, kind packet.PacketKind, data []byte) {
	s.pktQueue <- &PacketData{
		To:   cid,
		Kind: kind,
		Data: data,
	}
}

func (s *session) GetNewActiveSessionChannel() <-chan Connection {
	return s.newActiveSess
}

func (s *session) WaitNewActiveSession(timeout time.Duration) (Connection, error) {
	select {
	case active := <-s.newActiveSess:
		return active, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for new active session")
	}
}

func (s *session) WaitProbeInfo(timeout time.Duration) (*ProbeInfo, error) {
	select {
	case info := <-s.probeInfo:
		return info, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for probe info")
	}
}

func (s *session) GetConnection(cid ConnectionID) (Connection, bool) {
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()
	session, exists := s.activeSessions[cid]
	return session, exists
}

type Handshake = packet.Handshake

func unmapAddrPort(addr netip.AddrPort) netip.AddrPort {
	// for unifying ipv4 and ipv6 addresses
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}

func (s *session) sendHandshake(cid ConnectionID, priv []byte, hs *Handshake, conn *ActiveSession) (*ChanWithTimeout[Connection], error) {
	pkt := &packet.Packet{
		Header: packet.PacketHeader{
			Version:      0,
			ConnectionId: cid.ID,
			Kind:         packet.PacketKind_Handshake,
		},
	}
	data, err := hs.Append(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to encode handshake: %w", err)
	}
	pkt.Header.Len = uint16(len(data))
	if !pkt.SetData(data) {
		return nil, fmt.Errorf("dictionary too large")
	}
	p, err := pkt.Append(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to encode packet: %w", err)
	}
	hsDone := make(chan Connection, 1)
	sent, exists := s.sentHandshake[cid]
	if !exists {
		s.sentHandshake[cid] = &handshakeInfo{
			ConnectionID: cid,
			KeyKind:      hs.KeyKind,
			PrivateKey:   priv,
			LastTime:     time.Now(),
			Transcript:   p,
			hsDone:       hsDone,
			proxySession: conn,
		}
	} else {
		close(sent.hsDone)
		clear(sent.PrivateKey)
		if sent.proxySession != nil {
			sent.proxySession.closeUnlocked()
		}
		sent.KeyKind = hs.KeyKind
		sent.PrivateKey = priv
		sent.LastTime = time.Now()
		sent.Transcript = p
		sent.hsDone = hsDone
		sent.proxySession = conn
	}
	s.sendPacket(cid, packet.PacketKind_Handshake, p)
	s.logger.Debug("sent handshake", "cid", cid.String())
	return &ChanWithTimeout[Connection]{C: hsDone}, nil
}

func (s *session) sendRehandshakeForProxy(a *ActiveSession, priv []byte, hs *Handshake) (*ChanWithTimeout[Connection], error) {
	// deleting from active sessions temporary
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	if c, exists := s.activeSessions[a.cid]; !exists || c != a {
		return nil, fmt.Errorf("active session not found for rehandshake: %v", a.cid)
	}
	s.deleteActiveSession(a.cid)
	return s.sendHandshake(a.cid, priv, hs, a)
}

func (s *session) SendHandshake(cid ConnectionID, priv []byte, hs *Handshake) (*ChanWithTimeout[Connection], error) {
	if s.mode == SessionModeServer {
		return nil, fmt.Errorf("cannot send handshake in server mode")
	}
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	cid.Addr = unmapAddrPort(cid.Addr)
	if _, exists := s.activeSessions[cid]; exists {
		return nil, fmt.Errorf("session already exists for %v", cid)
	}
	return s.sendHandshake(cid, priv, hs, nil)
}

type HandshakeInfo struct {
	Addr     ConnectionID
	LastTime time.Time
}

func (s *session) DeleteInactiveSessionsBefore(limit time.Time) []Connection {
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	var deleted []Connection
	for addr, session := range s.activeSessions {
		if session.lastTime.Before(limit) {
			s.deleteActiveSession(addr)
			session.closeUnlocked()
			deleted = append(deleted, session)
			s.logger.Info("deleting inactive session", "cid", addr.String())
		}
	}
	return deleted
}

func (s *session) DeleteHandshakeBefore(limit time.Time) []HandshakeInfo {
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	var deleted []HandshakeInfo
	for addr, probe := range s.sentHandshake {
		if probe.LastTime.Before(limit) {
			probe.closeUnlocked()
			s.deleteHandshake(addr)
			deleted = append(deleted, HandshakeInfo{
				Addr:     addr,
				LastTime: probe.LastTime,
			})
			s.logger.Info("deleting expired handshake", "cid", addr.String())
		}
	}
	return deleted
}

type ProxyInfo struct {
	Peer1    ConnectionID
	Peer2    ConnectionID
	LastTime time.Time
}

func (s *session) DeleteProxyBefore(limit time.Time) []ProxyInfo {
	s.proxyLock.Lock()
	defer s.proxyLock.Unlock()
	var deleted []ProxyInfo
	seen := make(map[string]struct{})
	for _, setting := range s.proxySettings {
		if setting.lastUsed.Before(limit) {
			if _, ok := seen[setting.peer1.String()]; !ok {
				deleted = append(deleted, ProxyInfo{
					Peer1:    setting.peer1,
					Peer2:    setting.peer2,
					LastTime: setting.lastUsed,
				})
				seen[setting.peer1.String()] = struct{}{}
			}
			if _, ok := seen[setting.peer2.String()]; !ok {
				deleted = append(deleted, ProxyInfo{
					Peer1:    setting.peer1,
					Peer2:    setting.peer2,
					LastTime: setting.lastUsed,
				})
				seen[setting.peer2.String()] = struct{}{}
			}
			delete(s.proxySettings, setting.peer1)
			delete(s.proxySettings, setting.peer2)
			s.logger.Info("deleting expired proxy setting", "peer1", setting.peer1.String(), "peer2", setting.peer2.String())
		}
	}
	return deleted
}

type KeyInfo struct {
	MasterSecret     []byte
	HSIV             []byte
	AckIV            []byte
	HsHeaderProtect  []byte
	AckHeaderProtect []byte
}

func keySchedule(secret []byte, integrityInfo []byte) (keyInfo KeyInfo, err error) {
	preMasterSecret, err := DeriveKey(secret, "ksdk-protocol-session"+string(integrityInfo), 32)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("failed to derive pre-master secret: %w", err)
	}
	masterSecret, err := DeriveKey(preMasterSecret, "ksdk-protocol-master", 32)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("failed to derive master secret: %w", err)
	}
	hsIv, err := DeriveKey(preMasterSecret, "ksdk-protocol-nonce-hs", 12)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("failed to derive nonce IV: %w", err)
	}
	ackIv, err := DeriveKey(preMasterSecret, "ksdk-protocol-nonce-ack", 12)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("failed to derive ack nonce IV: %w", err)
	}
	ackHeaderProtect, err := DeriveKey(preMasterSecret, "ksdk-protocol-header-protect-ack", 32)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("failed to derive ack header protect: %w", err)
	}
	hsHeaderProtect, err := DeriveKey(preMasterSecret, "ksdk-protocol-header-protect-hs", 32)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("failed to derive hs header protect: %w", err)
	}
	return KeyInfo{
		MasterSecret:     masterSecret,
		HSIV:             hsIv,
		AckIV:            ackIv,
		HsHeaderProtect:  hsHeaderProtect,
		AckHeaderProtect: ackHeaderProtect,
	}, nil
}

func (s *session) addActiveSession(cid ConnectionID, aead cipher.AEAD,
	selfIV []byte, peerIV []byte,
	commonKeyKind packet.CommonKeyKind,
	selfHeaderProtect []byte, peerHeaderProtect []byte,
	transcript []byte, hsDone chan Connection, proxyConn *ActiveSession) error {
	var (
		selfHeaderProtectBlock cipher.Block
		peerHeaderProtectBlock cipher.Block
		err1, err2             error
	)
	switch commonKeyKind {
	case packet.CommonKeyKind_Aes128Gcm:
		selfHeaderProtectBlock, err1 = aes.NewCipher(selfHeaderProtect[:16])
		peerHeaderProtectBlock, err2 = aes.NewCipher(peerHeaderProtect[:16])
	case packet.CommonKeyKind_Aes192Gcm:
		selfHeaderProtectBlock, err1 = aes.NewCipher(selfHeaderProtect[:24])
		peerHeaderProtectBlock, err2 = aes.NewCipher(peerHeaderProtect[:24])
	case packet.CommonKeyKind_Aes256Gcm, packet.CommonKeyKind_Chacha20Poly1305:
		selfHeaderProtectBlock, err1 = aes.NewCipher(selfHeaderProtect[:32])
		peerHeaderProtectBlock, err2 = aes.NewCipher(peerHeaderProtect[:32])
	default:
		return fmt.Errorf("unsupported common key kind: %v", commonKeyKind)
	}
	if err1 != nil {
		return fmt.Errorf("failed to create self header protect cipher: %w", err1)
	}
	if err2 != nil {
		return fmt.Errorf("failed to create peer header protect cipher: %w", err2)
	}
	now := time.Now()
	var active *ActiveSession
	if proxyConn != nil {
		proxyConn.mu.Lock()
		defer proxyConn.mu.Unlock()
		active = proxyConn
		active.session = s
		active.connTime = now
		active.lastTime = now
		active.sessionSecret = aead
		active.selfIV = selfIV
		active.peerIV = peerIV
		active.selfHeaderProtect = selfHeaderProtectBlock
		active.peerHeaderProtect = peerHeaderProtectBlock
		active.transcript = transcript
		active.recvTracker.Reset()
		active.sentCounter.Store(0)
		active.proxied = true
	} else {
		active = &ActiveSession{
			session:           s,
			cid:               cid,
			connTime:          now,
			lastTime:          now,
			sessionSecret:     aead,
			selfIV:            selfIV,
			peerIV:            peerIV,
			selfHeaderProtect: selfHeaderProtectBlock,
			peerHeaderProtect: peerHeaderProtectBlock,
			transcript:        transcript,
			// newly created
			msgs:   NewMessageChannel(10, s.logger),
			closed: make(chan struct{}),
		}
	}
	s.activeSessions[cid] = active
	if hsDone != nil {
		hsDone <- active
	} else {
		s.newActiveSess <- active
	}
	s.logger.Info("new active session added", "cid", cid.String())
	return nil
}

func (s *session) receiveHandshake(cid ConnectionID, hs *packet.Handshake, originalPacket []byte) error {
	curve, err := CurveFromKeyKind(hs.KeyKind)
	if err != nil {
		s.logger.Error("failed to get curve from key kind", "cid", cid.String(), "error", err, "keyKind", hs.KeyKind)
		return fmt.Errorf("failed to get curve from key kind: %w", err)
	}
	priv, response, err := NewECDHHandshake(curve, hs.CommonKeyKind)
	if err != nil {
		return fmt.Errorf("failed to create ECDH probe: %w", err)
	}
	sharedSecret, commonKeyKind, err := ECDHFromHandshake(priv, hs)
	if err != nil {
		return fmt.Errorf("failed to derive shared secret: %w", err)
	}
	hsIntegrityInfo := integrityInfo(cid, hs)
	keys, err := keySchedule(sharedSecret, hsIntegrityInfo)
	if err != nil {
		return fmt.Errorf("failed to perform key schedule: %w", err)
	}
	aead, err := NewAEADFromCommonKeyKind(commonKeyKind, keys.MasterSecret)
	if err != nil {
		return fmt.Errorf("failed to create AEAD: %w", err)
	}
	clear(priv)
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	if _, exists := s.activeSessions[cid]; exists {
		s.logger.Warn("session already exists for handshake", "cid", cid.String())
		return fmt.Errorf("session already exists for %v", cid)
	}
	ackPkt := &packet.Packet{
		Header: packet.PacketHeader{
			Version:      0,
			ConnectionId: cid.ID,
			Kind:         packet.PacketKind_HandshakeAck,
		},
	}
	data, err := response.Append(nil)
	if err != nil {
		return fmt.Errorf("failed to encode handshake ack: %w", err)
	}
	ackPkt.Header.Len = uint16(len(data))
	ackPkt.Data = data
	ackData, err := ackPkt.Append(nil)
	if err != nil {
		return fmt.Errorf("failed to encode packet: %w", err)
	}
	err = s.addActiveSession(cid, aead, keys.AckIV, keys.HSIV, commonKeyKind, keys.AckHeaderProtect, keys.HsHeaderProtect, append(originalPacket, ackData...), nil, nil)
	if err != nil {
		s.logger.Error("failed to add active session", "cid", cid.String(), "error", err)
		return fmt.Errorf("failed to add active session: %w", err)
	}
	s.sendPacket(cid, packet.PacketKind_HandshakeAck, ackData)
	s.logger.Debug("sent handshake ack", "cid", cid.String())
	return nil
}

func integrityInfo(cid ConnectionID, hs *packet.Handshake) []byte {
	appended := []byte{}
	appended = append(appended, byte(cid.ID>>8), byte(cid.ID))
	appended = append(appended, byte(hs.KeyKind>>8), byte(hs.KeyKind))
	appended = append(appended, byte(hs.CommonKeyKind>>8), byte(hs.CommonKeyKind))
	return appended
}

func (s *session) receiveHandshakeAck(cid ConnectionID, hs *packet.Handshake, originalData []byte) (err error) {
	s.sessionsLock.Lock()
	defer s.sessionsLock.Unlock()
	sentProbes, exists := s.sentHandshake[cid]
	if !exists {
		s.logger.Warn("no sent probe for handshake ack", "cid", cid.String())
		return fmt.Errorf("no sent probe for %v", cid)
	}
	defer func() {
		if err != nil {
			s.logger.Warn("invalid handshake for cid", "cid", cid.String(), "error", err.Error())
		}
	}()
	sharedSecret, commonKeyKind, err := ECDHFromHandshake(sentProbes.PrivateKey, hs)
	if err != nil {
		return fmt.Errorf("failed to derive shared secret: %w", err)
	}
	hsIntegrityInfo := integrityInfo(cid, hs)
	keys, err := keySchedule(sharedSecret, hsIntegrityInfo)
	if err != nil {
		return fmt.Errorf("failed to perform key schedule: %w", err)
	}
	aead, err := NewAEADFromCommonKeyKind(commonKeyKind, keys.MasterSecret)
	if err != nil {
		return fmt.Errorf("failed to create AEAD: %w", err)
	}
	clear(sentProbes.PrivateKey) // only clear private key
	s.deleteHandshake(cid)
	err = s.addActiveSession(cid, aead, keys.HSIV, keys.AckIV, commonKeyKind, keys.HsHeaderProtect, keys.AckHeaderProtect, append(sentProbes.Transcript, originalData...), sentProbes.hsDone, sentProbes.proxySession)
	if err != nil {
		s.logger.Error("failed to add active session", "cid", cid.String(), "error", err)
		return fmt.Errorf("failed to add active session: %w", err)
	}
	return nil
}

func (s *session) receiveApplication(cid ConnectionID, data []byte, hdr *packet.PacketHeader) error {
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()
	activeSession, exists := s.activeSessions[cid]
	if !exists {
		s.logger.Warn("no active session for application data", "cid", cid.String())
		return fmt.Errorf("no active session for %v", cid)
	}
	activeSession.mu.Lock()
	defer activeSession.mu.Unlock()
	if len(data) < 8+16 {
		s.logger.Warn("application data too short for decryption", "cid", cid.String())
		return fmt.Errorf("data too short for decryption")
	}
	hdrData := hdr.MustAppend(nil)
	nonce := make([]byte, activeSession.sessionSecret.NonceSize())
	var sample [16]byte
	copy(sample[:], data[8:8+16])
	mask := headerProtectionMask(sample, activeSession.peerHeaderProtect)
	subtle.XORBytes(data[:8], mask[:], data[:8])
	nonceCounter := binary.BigEndian.Uint64(data[:8])
	if !activeSession.recvTracker.InsertNonce(nonceCounter, time.Now(), true) {
		s.logger.Warn("replay attack detected", "cid", cid.String(), "nonceCounter", nonceCounter, "lastCounter", activeSession.recvTracker.largestNonce)
		return fmt.Errorf("replay attack detected")
	}
	copy(nonce[4:], data[:8]) // Use first 8 bytes as nonce counter
	ciphertext := data[8:]
	subtle.XORBytes(nonce[:], activeSession.peerIV, nonce)
	plaintext, err := activeSession.sessionSecret.Open(ciphertext[:0], nonce, ciphertext, hdrData)
	if err != nil {
		s.logger.Warn("failed to decrypt application data", "cid", cid.String(), "error", err)
		return fmt.Errorf("failed to decrypt data: %w", err)
	}
	activeSession.recvTracker.InsertNonce(nonceCounter, time.Now(), false)
	activeSession.lastTime = time.Now()
	activeSession.msgs.SendMessage(Message{
		From:         cid,
		PacketNumber: nonceCounter,
		Data:         plaintext,
	})
	return nil
}

func (s *session) Receive(transport string, from netip.AddrPort, data []byte) error {
	// for unifying ipv4 and ipv6 addresses
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	if err := s.receive(transport, from, data); err != nil {
		s.logger.Debug("failed to receive packet", "from", from.String(), "error", err)
		return err
	}
	return nil
}

func (s *session) receiveProbe(cid ConnectionID, data []byte) error {
	probe := &packet.Probe{}
	if err := probe.DecodeExact(data); err != nil {
		return fmt.Errorf("failed to decode probe: %w", err)
	}
	addr, ok := netip.AddrFromSlice(probe.IpAddress.Address)
	if !ok {
		return fmt.Errorf("invalid ip address in probe")
	}
	s.logger.Info("received probe", "cid", cid.String(), "mac", probe.MacAddress, "ip", addr.String())
	select {
	case s.probeInfo <- &ProbeInfo{
		MacAddress: probe.MacAddress.Address,
		IpAddress:  netip.AddrPortFrom(addr, probe.Port),
		Sender:     cid,
	}:
	default:
		s.logger.Warn("probe info channel full, dropping probe", "cid", cid.String())
	}
	return nil
}

func (s *session) receive(transport string, from netip.AddrPort, data []byte) error {
	pkt := &packet.Packet{}
	if err := pkt.DecodeExact(data); err != nil {
		return fmt.Errorf("failed to decode packet: %w", err)
	}
	cid := NewConnectionID(transport, from, pkt.Header.ConnectionId)
	s.proxyLock.Lock()
	proxyTo, exists := s.proxySettings[cid]
	if exists {
		proxyTo.lastUsed = time.Now()
	}
	s.proxyLock.Unlock()
	if exists {
		peer, ok := proxyTo.getPeer(cid)
		if !ok {
			s.logger.Error("failed to get proxy peer", "cid", cid.String())
			return fmt.Errorf("failed to get proxy peer for %v", cid)
		}
		s.sendPacket(peer, pkt.Header.Kind, data)
		s.logger.Debug("proxied packet", "from", cid.String(), "to", peer.String(), "kind", pkt.Header.Kind)
		return nil
	}
	switch pkt.Header.Kind {
	case packet.PacketKind_Handshake, packet.PacketKind_HandshakeAck:
		if s.mode == SessionModeClient && pkt.Header.Kind == packet.PacketKind_Handshake {
			s.logger.Warn("client session received handshake packet, ignoring", "cid", cid.String())
			return nil
		}
		if s.mode == SessionModeServer && pkt.Header.Kind == packet.PacketKind_HandshakeAck {
			s.logger.Warn("server session received handshake ack packet, ignoring", "cid", cid.String())
			return nil
		}
		hs := &packet.Handshake{}
		if err := hs.DecodeExact(pkt.Data); err != nil {
			return fmt.Errorf("failed to decode handshake: %w", err)
		}
		if pkt.Header.Kind == packet.PacketKind_HandshakeAck {
			return s.receiveHandshakeAck(cid, hs, data)
		}
		return s.receiveHandshake(cid, hs, data)
	case packet.PacketKind_Application:
		return s.receiveApplication(cid, pkt.Data, &pkt.Header)
	case packet.PacketKind_Probe:
		return s.receiveProbe(cid, pkt.Data)
	default:
		return fmt.Errorf("unknown packet kind: %v", pkt.Header.Kind)
	}
}

func headerProtectionMask(sample [16]byte, headerProtectKey cipher.Block) [16]byte {
	var mask [16]byte
	headerProtectKey.Encrypt(mask[:], sample[:])
	return mask
}

func (s *session) sendApplication(cid ConnectionID, data []byte, a *ActiveSession, pn *PacketNumber) (int, PacketNumber, error) {
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()
	activeSession, exists := s.activeSessions[cid]
	if !exists {
		return 0, 0, fmt.Errorf("no active session for %v", cid)
	}
	if activeSession != a {
		return 0, 0, fmt.Errorf("active session mismatch for %v", cid)
	}
	activeSession.mu.Lock()
	defer activeSession.mu.Unlock()
	pkt := &packet.Packet{
		Header: packet.PacketHeader{
			Version:      0,
			Kind:         packet.PacketKind_Application,
			ConnectionId: cid.ID,
		},
	}
	pktLen := 8 + len(data) + activeSession.sessionSecret.Overhead()
	if pktLen > 0xffff {
		return 0, 0, fmt.Errorf("data too large to send")
	}
	pkt.Header.Len = uint16(pktLen)
	plaintext := data
	nonce := make([]byte, activeSession.sessionSecret.NonceSize())
	var count uint64
	if pn != nil {
		count = uint64(*pn)
	} else {
		count = uint64(activeSession.sentCounter.Add(1)) // from 1
	}
	copy(nonce[4:], binary.BigEndian.AppendUint64(nil, count))
	hdrData := pkt.Header.MustAppend(nil)
	finalData := make([]byte, pktLen)
	copy(finalData[:8], nonce[4:])
	subtle.XORBytes(nonce[:], activeSession.selfIV, nonce)
	activeSession.sessionSecret.Seal(finalData[8:8], nonce, plaintext, hdrData)
	sample := [16]byte{}
	copy(sample[:], finalData[8:8+16])
	mask := headerProtectionMask(sample, activeSession.selfHeaderProtect)
	subtle.XORBytes(finalData[:8], mask[:], finalData[:8])
	pkt.Data = finalData
	pktData, err := pkt.Append(nil)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to encode packet: %w", err)
	}
	pktLength := len(pktData)
	s.sendPacket(cid, packet.PacketKind_Application, pktData)
	s.logger.Debug("sent application packet", "cid", cid.String())
	return pktLength, count, nil
}

func (s *session) SendProbe(cid ConnectionID, macAddr [6]byte, ipAddr netip.AddrPort) error {
	data, err := s.makeProbe(cid.ID, macAddr, ipAddr)
	if err != nil {
		return fmt.Errorf("failed to make probe response: %w", err)
	}
	s.sendPacket(cid, packet.PacketKind_Probe, data)
	s.logger.Debug("sent probe", "cid", cid.String())
	return nil
}

func (s *session) makeProbe(probeID uint16, mac [6]byte, ipAddr netip.AddrPort) ([]byte, error) {
	probe := &packet.Probe{}
	probe.MacAddress = packet.MacAddress{Address: mac}
	if !probe.IpAddress.SetAddress(ipAddr.Addr().AsSlice()) {
		return nil, fmt.Errorf("invalid ip address")
	}
	probe.Port = ipAddr.Port()
	data, err := probe.Append(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to encode probe: %w", err)
	}
	pkt := &packet.Packet{
		Header: packet.PacketHeader{
			Version:      0,
			ConnectionId: probeID,
			Kind:         packet.PacketKind_Probe,
		},
	}
	pkt.Header.Len = uint16(len(data))
	pkt.Data = data
	pktData, err := pkt.Append(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to encode packet: %w", err)
	}
	return pktData, nil
}

func (s *session) GetProbeInfoChannel() <-chan *ProbeInfo {
	return s.probeInfo
}

func (s *session) ListActiveConnections() []Connection {
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()
	connections := make([]Connection, 0, len(s.activeSessions))
	for _, conn := range s.activeSessions {
		connections = append(connections, conn)
	}
	return connections
}

// transport detected that the packet cannot be sent
func (s *session) CannotSend(pkt *PacketData) {
	s.closeCannotSend(pkt)
}
func (s *session) ListHandshakes() []HandshakeInfo {
	s.sessionsLock.RLock()
	defer s.sessionsLock.RUnlock()
	var list []HandshakeInfo
	for addr, probe := range s.sentHandshake {
		list = append(list, HandshakeInfo{
			Addr:     addr,
			LastTime: probe.LastTime,
		})
	}
	return list
}

func (s *session) ListProxies() []ProxyInfo {
	s.proxyLock.Lock()
	defer s.proxyLock.Unlock()
	seen := make(map[string]struct{})
	var list []ProxyInfo
	for _, setting := range s.proxySettings {
		if _, ok := seen[setting.peer1.String()]; !ok {
			list = append(list, ProxyInfo{
				Peer1:    setting.peer1,
				Peer2:    setting.peer2,
				LastTime: setting.lastUsed,
			})
			seen[setting.peer1.String()] = struct{}{}
			seen[setting.peer2.String()] = struct{}{}
		}
		if _, ok := seen[setting.peer2.String()]; !ok {
			list = append(list, ProxyInfo{
				Peer1:    setting.peer1,
				Peer2:    setting.peer2,
				LastTime: setting.lastUsed,
			})
			seen[setting.peer2.String()] = struct{}{}
			seen[setting.peer1.String()] = struct{}{}
		}
	}
	return list
}
