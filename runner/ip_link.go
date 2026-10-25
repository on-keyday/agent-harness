package runner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/iplink"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ipLinks is the runner's registry of ip links, keyed by the server-assigned
// forward id: the third datagram registry beside the udp -L and -R ones.
type ipLinks struct {
	mu sync.Mutex
	m  map[uint64]*iplink.Link
	// closed is set by closeAll: the connection holding these links has
	// ended, and a link registered after that would be owned by nothing.
	closed bool
}

// add registers l, and reports false when the registry was already released
// -- the caller then owns l and must close it.
func (r *ipLinks) add(id uint64, l *iplink.Link) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.m == nil {
		r.m = map[uint64]*iplink.Link{}
	}
	r.m[id] = l
	return true
}

func (r *ipLinks) get(id uint64) (*iplink.Link, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.m[id]
	return l, ok
}

// remove takes the link out of the registry and returns it; the caller closes
// it. Returns nil when no link holds the id.
func (r *ipLinks) remove(id uint64) *iplink.Link {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.m[id]
	delete(r.m, id)
	return l
}

// closeAll releases every link: the connection holding them has ended.
func (r *ipLinks) closeAll() {
	r.mu.Lock()
	links := r.m
	r.m = nil
	r.closed = true
	r.mu.Unlock()
	for _, l := range links {
		_ = l.Close()
	}
}

func (r *ipLinks) dropsFor(id uint64) (protocol.ForwardDropsBody, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.m[id]; ok {
		return l.Drops.Snapshot(id), true
	}
	return protocol.ForwardDropsBody{}, false
}

// ipLinkRegistry returns the session's link registry, created once. Asked
// concurrently from the open path, the datagram pump and the close dispatch,
// so the creation is guarded rather than a bare nil check.
func (s *Session) ipLinkRegistry() *ipLinks {
	s.ipLinksOnce.Do(func() { s.ipLinks = &ipLinks{} })
	return s.ipLinks
}

// startIPLink brings up this runner's end of an ip link and answers the server
// either way. The server holds the registration until it hears, so the
// client's tun is never up while this end is missing.
func (s *Session) startIPLink(ctx context.Context, req *protocol.RunnerOpenPortForwardRequest) {
	log := s.logger()
	fail := func(detail string) {
		log.Info("ip link: setup failed", "fwd", req.ForwardId, "detail", detail)
		s.sendSetupResult(req.ForwardId, false, detail)
	}
	if s.worktreeDirFor(hex.EncodeToString(req.TaskId.Id[:])) == "" {
		fail("unknown task")
		return
	}
	if s.Sender == nil {
		fail("no sender wired")
		return
	}
	cfg := req.Link()
	var err error
	if cfg == nil {
		err = errors.New("no link configuration")
	} else {
		err = cfg.Validate()
	}
	if err != nil {
		fail("malformed link request: " + err.Error())
		return
	}
	open := s.openTun
	if open == nil {
		open = iplink.OpenTun
	}
	addr := cfg.RunnerPrefix()
	dev, err := open(addr, int(cfg.Mtu))
	if err != nil {
		if errors.Is(err, iplink.ErrNotLinux) {
			fail(fmt.Sprintf("%v (this runner is %s)", err, runtime.GOOS))
			return
		}
		fail(err.Error())
		return
	}
	l := iplink.NewLink(req.ForwardId, dev, s.Sender)
	if !s.ipLinkRegistry().add(req.ForwardId, l) {
		// The connection ended while the device was being opened. Nothing
		// would ever close a link registered on this session now.
		_ = l.Close()
		fail("connection ended during setup")
		return
	}
	s.sendSetupResult(req.ForwardId, true, "")
	log.Info(iplink.UpLine(l.Name(), addr, int(cfg.Mtu)), "fwd", req.ForwardId)
	go func() {
		if err := l.Run(ctx); err != nil {
			s.endIPLink(req.ForwardId, err)
		}
	}()
}

// endIPLink ends a link whose device failed after setup, and tells the server,
// which tears the forward down and tells the client. A link the server closed
// is already out of the registry, so this is a no-op for it.
func (s *Session) endIPLink(id uint64, err error) {
	l := s.ipLinkRegistry().remove(id)
	if l == nil {
		return
	}
	_ = l.Close()
	s.logger().Info("ip link: ended", "fwd", id, "err", err)
	s.sendForwardEnded(id, err.Error())
}

// sendForwardEnded reports that this runner's end of a forward stopped working
// after setup succeeded.
func (s *Session) sendForwardEnded(forwardID uint64, detail string) {
	m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_ForwardEnded}
	fe := protocol.ForwardEnded{ForwardId: forwardID}
	fe.SetDetail([]byte(detail))
	m.SetForwardEnded(fe)
	_ = s.Sender.Send(m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
}

// releaseForwardState drops every forward this session holds runner-side state
// for: -R listeners, udp registrations and their sockets, ip links. Called when
// the connection ends -- the server reads that as runner_gone and ends those
// forwards, and a reconnect builds a fresh Session that holds none of them.
func (s *Session) releaseForwardState() {
	if s.rforwards != nil {
		s.rforwards.closeAll()
	}
	if s.udpForwards != nil {
		s.udpForwards.closeAll()
	}
	// Through the accessor, so a registry not created yet is created closed:
	// a link whose setup is still opening its device must not land after this.
	s.ipLinkRegistry().closeAll()
}
