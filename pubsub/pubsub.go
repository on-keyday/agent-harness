package pubsub

import (
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

func JoinTopic(topic string) []byte {
	return (&protocol.PubSubRequest{
		Kind:  protocol.MessageKind_JOIN,
		Topic: []byte(topic),
	}).MustAppend([]byte{byte(wire.ApplicationPayloadKind_Control)})
}

func LeaveTopic(topic string) []byte {
	return (&protocol.PubSubRequest{
		Kind:  protocol.MessageKind_LEAVE,
		Topic: []byte(topic),
	}).MustAppend([]byte{byte(wire.ApplicationPayloadKind_Control)})
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
		return ps.Subscribe(topic, s).MustAppend(nil)
	case protocol.MessageKind_LEAVE:
		topic := string(req.Topic)
		return ps.Unsubscribe(topic, s).MustAppend(nil)
	}
	return (&protocol.PubSubResponse{
		Status: protocol.Status_UnknownMessage,
	}).MustAppend(nil)
}

type Subscriber struct {
	transport trsf.Transport
	topics    map[string]trsf.BidirectionalStream
}

func NewSubscriber(transport trsf.Transport) *Subscriber {
	return &Subscriber{
		transport: transport,
		topics:    make(map[string]trsf.BidirectionalStream),
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
}

func NewPubSub() *PubSub {
	return &PubSub{
		topics: make(map[string]*SubscriberList),
	}
}

func (ps *PubSub) Subscribe(topic string, sub *Subscriber) *protocol.PubSubResponse {
	ps.m.Lock()
	defer ps.m.Unlock()
	if sub.topics == nil {
		sub.topics = make(map[string]trsf.BidirectionalStream)
	}
	if id, ok := sub.topics[topic]; ok {
		return &protocol.PubSubResponse{
			Status:   protocol.Status_AlreadySubscribed,
			StreamId: uint64(id.ID()),
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
			slog.Info("received data from subscriber stream", "topic", topic, "length", len(data), "eof", eof)
			if len(data) != 0 {
				ps.Publish(topic, data)
			}
			if eof {
				return
			}
		}
	}()
	sub.topics[topic] = stream
	if _, ok := ps.topics[topic]; !ok {
		ps.topics[topic] = &SubscriberList{}
	}
	ps.topics[topic].AddSubscriber(sub)
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
			stream.CloseBoth()
			delete(sub.topics, topic)
			return &protocol.PubSubResponse{
				Status:   protocol.Status_Ok,
				StreamId: uint64(stream.ID()),
			}
		}
	}
	return &protocol.PubSubResponse{
		Status: protocol.Status_UnknownTopic,
	}

}

func (ps *PubSub) Publish(topic string, msg []byte) {
	ps.m.Lock()
	defer ps.m.Unlock()
	if sl, ok := ps.topics[topic]; ok {
		for _, sub := range sl.subscribers {
			stream, ok := sub.topics[topic]
			if !ok {
				continue
			}
			stream.Write(msg)
		}
	}
}
