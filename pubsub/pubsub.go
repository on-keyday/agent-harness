package pubsub

import (
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/objtrsf/objproto"
)

func JoinTopic(reqID uint32, nickName string, topic string) []byte {
	p := (&protocol.PubSubRequest{
		Kind:      protocol.MessageKind_JOIN,
		RequestId: reqID,
		Topic:     []byte(topic),
	})
	if !p.SetNickName([]uint8(nickName)) {
		return nil
	}
	return p.MustAppend([]byte{byte(appwire.AppKind_Pubsub)})
}

func LeaveTopic(reqID uint32, topic string) []byte {
	return (&protocol.PubSubRequest{
		Kind:      protocol.MessageKind_LEAVE,
		RequestId: reqID,
		Topic:     []byte(topic),
	}).MustAppend([]byte{byte(appwire.AppKind_Pubsub)})
}

func (s *Subscriber) HandleMessage(ps *PubSub, msg []byte) []byte {
	req := &protocol.PubSubRequest{}
	msg, err := req.Decode(msg)
	if err != nil {
		return nil
	}
	switch req.Kind {
	case protocol.MessageKind_JOIN:
		topic := string(req.Topic)
		return ps.Subscribe(req.RequestId, topic, string(*req.NickName()), s).MustAppend([]byte{byte(appwire.AppKind_Pubsub)})
	case protocol.MessageKind_LEAVE:
		topic := string(req.Topic)
		return ps.Unsubscribe(req.RequestId, topic, s).MustAppend([]byte{byte(appwire.AppKind_Pubsub)})
	}
	return (&protocol.PubSubResponse{
		Status: protocol.Status_UnknownMessage,
	}).MustAppend([]byte{byte(appwire.AppKind_Pubsub)})
}

func (s *Subscriber) LeaveAll(ps *PubSub) {
	for topic := range s.topics {
		ps.Unsubscribe(0, topic, s)
	}
}

type topicJoinInfo struct {
	name string
	conn trsf.BidirectionalStream
}

type Subscriber struct {
	id        objproto.ConnectionID
	transport trsf.Transport
	topics    map[string]*topicJoinInfo
}

func NewSubscriber(id objproto.ConnectionID, transport trsf.Transport) *Subscriber {
	return &Subscriber{
		id:        id,
		transport: transport,
		topics:    make(map[string]*topicJoinInfo),
	}
}

type SubscriberList struct {
	subscribers []*Subscriber
}

func (sl *SubscriberList) AddSubscriber(sub *Subscriber) {
	sl.subscribers = append(sl.subscribers, sub)
}

func (sl *SubscriberList) RemoveSubscriber(sub *Subscriber) {
	for i, s := range sl.subscribers {
		if s == sub {
			sl.subscribers = append(sl.subscribers[:i], sl.subscribers[i+1:]...)
			return
		}
	}
}

// Tap is a server-internal subscriber that receives raw published bytes for a topic
// without setting up a transport-level stream. Used for in-process logging / persistence.
type Tap struct {
	cb func(nickName string, msg []byte)
}

type PubSub struct {
	m      sync.Mutex
	topics map[string]*SubscriberList
	taps   map[string][]*Tap
	logger *slog.Logger
}

func NewPubSub(logger *slog.Logger) *PubSub {
	return &PubSub{
		topics: make(map[string]*SubscriberList),
		logger: logger,
	}
}

// TapSubscribe registers a callback for topic. Returns the Tap handle for later removal.
func (ps *PubSub) TapSubscribe(topic string, cb func(nickName string, msg []byte)) *Tap {
	t := &Tap{cb: cb}
	ps.m.Lock()
	defer ps.m.Unlock()
	if ps.taps == nil {
		ps.taps = make(map[string][]*Tap)
	}
	ps.taps[topic] = append(ps.taps[topic], t)
	return t
}

// TapUnsubscribe removes a previously-registered tap.
func (ps *PubSub) TapUnsubscribe(topic string, t *Tap) {
	ps.m.Lock()
	defer ps.m.Unlock()
	sl := ps.taps[topic]
	for i, tap := range sl {
		if tap == t {
			ps.taps[topic] = append(sl[:i], sl[i+1:]...)
			return
		}
	}
}

func (ps *PubSub) Subscribe(requestID uint32, topic string, nickName string, sub *Subscriber) *protocol.PubSubResponse {
	ps.m.Lock()
	defer ps.m.Unlock()
	if sub.topics == nil {
		sub.topics = make(map[string]*topicJoinInfo)
	}
	if id, ok := sub.topics[topic]; ok {
		return &protocol.PubSubResponse{
			Status:    protocol.Status_AlreadySubscribed,
			RequestId: requestID,
			StreamId:  uint64(id.conn.ID()),
		}
	}
	// Stream identification is by stream_id, returned in the PubSubResponse
	// below — subscribers/publishers look it up via Transport.GetBidirectional
	// Stream(id). No "<topic>\n" preamble: the legacy header was a leftover
	// from before request_id / stream_id correlation existed.
	stream := sub.transport.CreateBidirectionalStream()
	go func() {
		for {
			data, eof, err := stream.ReadDirect(trsf.InitialFlowWindow)
			if err != nil {
				return
			}
			ps.logger.Info("received data from subscriber stream", "topic", topic, "length", len(data), "eof", eof)
			if len(data) != 0 {
				ps.Publish(nickName, topic, data)
			}
			if eof {
				return
			}
		}
	}()
	sub.topics[topic] = &topicJoinInfo{
		name: nickName,
		conn: stream,
	}
	if _, ok := ps.topics[topic]; !ok {
		ps.topics[topic] = &SubscriberList{}
	}
	ps.topics[topic].AddSubscriber(sub)
	ps.logger.Info("subscriber joined topic", "topic", topic, "nickName", nickName, "subscriberID", sub.id, "streamID", stream.ID())
	return &protocol.PubSubResponse{
		Status:    protocol.Status_Ok,
		RequestId: requestID,
		StreamId:  uint64(stream.ID()),
	}
}

func (ps *PubSub) Unsubscribe(requestID uint32, topic string, sub *Subscriber) *protocol.PubSubResponse {
	ps.m.Lock()
	defer ps.m.Unlock()
	if sl, ok := ps.topics[topic]; ok {
		sl.RemoveSubscriber(sub)
		if len(sl.subscribers) == 0 {
			delete(ps.topics, topic)
		}
		if stream, ok := sub.topics[topic]; ok {
			stream.conn.CloseBoth()
			delete(sub.topics, topic)
			ps.logger.Info("subscriber left topic", "topic", topic, "nickName", stream.name, "subscriberID", sub.id, "streamID", stream.conn.ID())
			return &protocol.PubSubResponse{
				Status:    protocol.Status_Ok,
				RequestId: requestID,
				StreamId:  uint64(stream.conn.ID()),
			}
		}
	}
	return &protocol.PubSubResponse{
		Status:    protocol.Status_UnknownTopic,
		RequestId: requestID,
	}

}

func (ps *PubSub) Publish(nickName string, topic string, msg []byte) {
	ps.m.Lock()
	if sl, ok := ps.topics[topic]; ok {
		for _, sub := range sl.subscribers {
			stream, ok := sub.topics[topic]
			if !ok {
				continue
			}
			stream.conn.AppendData(false, msg)
		}
	}
	var tapsCopy []*Tap
	if ts, ok := ps.taps[topic]; ok {
		tapsCopy = append(tapsCopy, ts...) // snapshot
	}
	ps.m.Unlock()
	for _, t := range tapsCopy {
		t.cb(nickName, msg)
	}
}
