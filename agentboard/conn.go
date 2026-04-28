package agentboard

import "sync"

// ConnState is per-attached-client subscription state.
type ConnState struct {
	mu       sync.Mutex
	patterns map[string]struct{} // exact topic strings (glob is v2)
	notify   chan struct{}        // pinged when a relevant publish happens
}

func newConnState() *ConnState {
	return &ConnState{patterns: make(map[string]struct{}), notify: make(chan struct{}, 1)}
}

func (c *ConnState) addPattern(p string) {
	c.mu.Lock()
	c.patterns[p] = struct{}{}
	c.mu.Unlock()
}

func (c *ConnState) removePattern(p string) {
	c.mu.Lock()
	delete(c.patterns, p)
	c.mu.Unlock()
}

func (c *ConnState) matches(topic string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.patterns[topic]
	return ok
}

func (c *ConnState) ping() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}
