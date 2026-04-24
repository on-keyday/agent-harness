package pubsub

import (
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

func JoinTopic(nickName string, topic string) []byte {
	p := (&protocol.PubSubRequest{
		Kind:  protocol.MessageKind_JOIN,
		Topic: []byte(topic),
	})
	if !p.SetNickName([]uint8(nickName)) {
		return nil
	}
	return p.MustAppend([]byte{byte(wire.ApplicationPayloadKind_Pubsub)})
}

func LeaveTopic(topic string) []byte {
	return (&protocol.PubSubRequest{
		Kind:  protocol.MessageKind_LEAVE,
		Topic: []byte(topic),
	}).MustAppend([]byte{byte(wire.ApplicationPayloadKind_Pubsub)})
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
		return ps.Subscribe(topic, string(*req.NickName()), s).MustAppend(nil)
	case protocol.MessageKind_LEAVE:
		topic := string(req.Topic)
		return ps.Unsubscribe(topic, s).MustAppend(nil)
	}
	return (&protocol.PubSubResponse{
		Status: protocol.Status_UnknownMessage,
	}).MustAppend(nil)
}

func (s *Subscriber) LeaveAll(ps *PubSub) {
	for topic := range s.topics {
		ps.Unsubscribe(topic, s)
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

type PubSub struct {
	m      sync.Mutex
	topics map[string]*SubscriberList
	logger *slog.Logger
}

func NewPubSub(logger *slog.Logger) *PubSub {
	return &PubSub{
		topics: make(map[string]*SubscriberList),
		logger: logger,
	}
}

func (ps *PubSub) Subscribe(topic string, nickName string, sub *Subscriber) *protocol.PubSubResponse {
	ps.m.Lock()
	defer ps.m.Unlock()
	if sub.topics == nil {
		sub.topics = make(map[string]*topicJoinInfo)
	}
	if id, ok := sub.topics[topic]; ok {
		return &protocol.PubSubResponse{
			Status:   protocol.Status_AlreadySubscribed,
			StreamId: uint64(id.conn.ID()),
		}
	}
	stream := sub.transport.CreateBidirectionalStream()
	stream.AppendData(false, []byte(topic), []byte("\n"))
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
		Status:   protocol.Status_Ok,
		StreamId: uint64(stream.ID()),
	}
}

func (ps *PubSub) Unsubscribe(topic string, sub *Subscriber) *protocol.PubSubResponse {
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
				Status:   protocol.Status_Ok,
				StreamId: uint64(stream.conn.ID()),
			}
		}
	}
	return &protocol.PubSubResponse{
		Status: protocol.Status_UnknownTopic,
	}

}

func (ps *PubSub) Publish(nickName string, topic string, msg []byte) {
	ps.m.Lock()
	defer ps.m.Unlock()
	if sl, ok := ps.topics[topic]; ok {
		for _, sub := range sl.subscribers {
			stream, ok := sub.topics[topic]
			if !ok {
				continue
			}
			stream.conn.AppendData(false, msg)
		}
	}
}
