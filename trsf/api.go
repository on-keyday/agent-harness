package trsf

import (
	"context"
	"io"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

type StreamID uint64

func (s StreamID) Next() StreamID {
	return s + 4
}

const (
	ServerBidirectionalStart  StreamID = 0
	ClientBidirectionalStart  StreamID = 1
	ServerUnidirectionalStart StreamID = 2
	ClientUnidirectionalStart StreamID = 3
)

func (s StreamID) IsServerInitiated() bool {
	return s%2 == 0
}

func (s StreamID) IsClientInitiated() bool {
	return s%2 == 1
}

func (s StreamID) IsBidirectional() bool {
	return s%4 < 2
}

func (s StreamID) IsUnidirectional() bool {
	return s%4 >= 2
}

type SendStream interface {
	ID() StreamID
	io.WriteCloser
	WriteContext(ctx context.Context, data []byte) (n int, err error)
	HasSendData() bool
	Completed() bool
	AppendData(eof bool, data ...[]byte) error
	AppendDataContext(ctx context.Context, eof bool, data ...[]byte) error
}

type ReceiveStream interface {
	ID() StreamID
	io.Reader
	ReadContext(ctx context.Context, p []byte) (n int, err error)
	HasRecvData() bool
	EOF() bool
	Cancel()
}

type BidirectionalStream interface {
	SendStream
	ReceiveStream
	CloseBoth() error
}

type Multiplexer interface {
	CreateBidirectionalStream() BidirectionalStream
	CreateSendStream() SendStream
	AcceptBidirectionalStream(ctx context.Context) (BidirectionalStream, error)
	AcceptReceiveStream(ctx context.Context) (ReceiveStream, error)
	GetInternalState() *InternalState
}

type Transport interface {
	Multiplexer
	Send(msg *objproto.Message)
	Recv(ctx context.Context) *SendAction
}

func AutoSend(ctx context.Context, p Transport, conn UnderlayingSendTransport, onEnd func(err error)) {
	for {
		action := p.Recv(ctx)
		if action == nil {
			if onEnd != nil {
				onEnd(nil)
			}
			return
		}
		err := action.Send(ctx, conn)
		if err != nil {
			if onEnd != nil {
				onEnd(err)
			}
			return
		}
	}
}

type UnderlayingReceiveTransport interface {
	ReceiveMessageContext(ctx context.Context) (*objproto.Message, error)
}

func AutoReceive(ctx context.Context, p Transport, conn UnderlayingReceiveTransport, onEvent func(msg *objproto.Message, err error)) {
	for {
		data, err := conn.ReceiveMessageContext(ctx)
		if err != nil {
			if onEvent != nil {
				onEvent(nil, err)
			}
			return
		}
		if len(data.Data) == 0 {
			continue
		}
		if wire.IsStreamRelated(wire.ApplicationPayloadKind(data.Data[0])) {
			p.Send(data)
			continue
		}
		if onEvent != nil {
			onEvent(data, nil)
		}
	}
}
