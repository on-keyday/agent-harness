package pubsub

import "github.com/on-keyday/agent-harness/objproto"

type Subscriber struct {
	Conn   objproto.Connection
	Topics map[string]struct{}
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
	conns map[string]*SubscriberList
}

type Message struct {
	Topic string
	Data  string
}
