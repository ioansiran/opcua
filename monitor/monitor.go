package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
)

var (
	// DefaultMaxChanLen is the size of the internal buffer when using a callback-based subscription
	DefaultMaxChanLen = 8192

	// ErrSlowConsumer is returned when a subscriber does not keep up with the incoming messages
	ErrSlowConsumer = errors.New("opcua: slow consumer. messages dropped")
)

// ErrHandler is a function that is called when there is an out of band issue with delivery
type ErrHandler func(*opcua.Client, *Subscription, error)

// MsgHandler is a function that is called for each new DataValue
type MsgHandler func(*ua.NodeID, *ua.DataValue)

// DataChangeMessage represents the changed DataValue from the server. It also includes a reference
// to the sending NodeID and error (if any)
type DataChangeMessage struct {
	*ua.DataValue
	Error  error
	NodeID *ua.NodeID
}

// NodeMonitor creates new subscriptions
type NodeMonitor struct {
	client           *opcua.Client
	nextClientHandle uint32
	errHandlerCB     ErrHandler
}

// Subscription is an instance of an active subscription.
// Nodes can be added and removed concurrently.
type Subscription struct {
	monitor          *NodeMonitor
	sub              *opcua.Subscription
	notifyCh         chan *DataChangeMessage
	internalNotifyCh chan *opcua.PublishNotificationData
	delivered        uint64
	dropped          uint64
	closed           chan struct{}
	mu               sync.RWMutex
	handles          map[uint32]*ua.NodeID
	nodeLookup       map[string]uint32
}

// New creates a new NodeMonitor
func New(client *opcua.Client) (*NodeMonitor, error) {
	m := &NodeMonitor{
		client:           client,
		nextClientHandle: 100,
	}

	return m, nil
}

func newSubscription(m *NodeMonitor) (*Subscription, error) {
	return &Subscription{
		monitor:          m,
		closed:           make(chan struct{}),
		internalNotifyCh: make(chan *opcua.PublishNotificationData),
		handles:          make(map[uint32]*ua.NodeID),
		nodeLookup:       make(map[string]uint32),
	}, nil
}

// SetErrorHandler sets an optional callback for async errors
func (m *NodeMonitor) SetErrorHandler(cb ErrHandler) {
	m.errHandlerCB = cb
}

// Subscribe creates a new callback-based subscription and an optional list of nodes.
// The caller must call `Unsubscribe` to stop and clean up resources. Canceling the context
// will also cause the subscription to stop, but `Unsubscribe` must still be called.
func (m *NodeMonitor) Subscribe(ctx context.Context, cb MsgHandler, nodes ...string) (*Subscription, error) {
	ch := make(chan *DataChangeMessage, DefaultMaxChanLen)

	sub, err := m.ChanSubscribe(ctx, ch, nodes...)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.closed:
				return
			case msg := <-ch:
				if msg.Error != nil {
					sub.sendError(msg.Error)
				} else {
					cb(msg.NodeID, msg.DataValue)
				}
			}
		}
	}()

	return sub, nil
}

// ChanSubscribe creates a new channel-based subscription and an optional list of nodes.
// The channel should be deep enough to allow some buffering, otherwise `ErrSlowConsumer` is sent
// via the monitor's `ErrHandler`.
// The caller must call `Unsubscribe` to stop and clean up resources. Canceling the context
// will also cause the subscription to stop, but `Unsubscribe` must still be called.
func (m *NodeMonitor) ChanSubscribe(ctx context.Context, ch chan *DataChangeMessage, nodes ...string) (*Subscription, error) {
	s, err := newSubscription(m)
	if err != nil {
		return nil, err
	}

	s.notifyCh = ch

	if s.sub, err = m.client.Subscribe(&opcua.SubscriptionParameters{
		Notifs: s.internalNotifyCh,
	}); err != nil {
		return nil, err
	}

	if err = s.AddNodes(nodes...); err != nil {
		return nil, err
	}

	go s.pump(ctx)
	go s.sub.Run(ctx)

	return s, nil
}

func (s *Subscription) sendError(err error) {
	if err != nil && s.monitor.errHandlerCB != nil {
		s.monitor.errHandlerCB(s.monitor.client, s, err)
	}
}

// internal func to read from internal channel and write to client provided channel
func (s *Subscription) pump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case msg := <-s.internalNotifyCh:
			if msg.Error != nil {
				// TODO: is it possible to have an error _and_ some DataChangeNotification values?
				s.sendError(msg.Error)
				continue
			}

			if msg.SubscriptionID != s.sub.SubscriptionID {
				panic("wtf!?")
			}

			switch v := msg.Value.(type) {
			case *ua.DataChangeNotification:
				for _, item := range v.MonitoredItems {
					s.mu.RLock()
					nid, ok := s.handles[item.ClientHandle]
					s.mu.RUnlock()

					out := &DataChangeMessage{}

					if !ok {
						out.Error = fmt.Errorf("handle %d not found", item.ClientHandle)
						// TODO: should the error also propogate via the monitor callback?
					} else {
						out.NodeID = nid
						out.DataValue = item.Value
					}

					select {
					case s.notifyCh <- out:
						atomic.AddUint64(&s.delivered, 1)
					default:
						atomic.AddUint64(&s.dropped, 1)
						s.sendError(ErrSlowConsumer)
					}
				}
			default:
				s.sendError(fmt.Errorf("unknown message type: %T", msg.Value))
			}
		}
	}
}

// Unsubscribe removes the subscription interests and cleans up any resources
func (s *Subscription) Unsubscribe() error {
	// TODO: make idempotent
	close(s.closed)
	return s.sub.Cancel()
}

// Delivered returns the number of DataChangeMessages delivered
func (s *Subscription) Delivered() uint64 {
	return atomic.LoadUint64(&s.delivered)
}

// Dropped returns the number of DataChangeMessages dropped due to a slow consumer
func (s *Subscription) Dropped() uint64 {
	return atomic.LoadUint64(&s.dropped)
}

// AddNodes adds nodes defined by their string representation
func (s *Subscription) AddNodes(nodes ...string) error {
	nodeIDs, err := parseNodeSlice(nodes...)
	if err != nil {
		return err
	}
	return s.AddNodeIDs(nodeIDs...)
}

// AddNodeIDs adds nodes
func (s *Subscription) AddNodeIDs(nodes ...*ua.NodeID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	toAdd := make([]*ua.MonitoredItemCreateRequest, 0)

	for _, node := range nodes {
		handle := atomic.AddUint32(&s.monitor.nextClientHandle, 1)

		s.handles[handle] = node
		s.nodeLookup[node.String()] = handle

		toAdd = append(toAdd, opcua.NewMonitoredItemCreateRequestWithDefaults(node, ua.AttributeIDValue, handle))
	}

	resp, err := s.sub.Monitor(ua.TimestampsToReturnBoth, toAdd...)
	if err != nil {
		return err
	}

	if resp.ResponseHeader.ServiceResult != ua.StatusOK {
		return resp.ResponseHeader.ServiceResult
	}

	return nil
}

// RemoveNodes removes nodes defined by their string representation
func (s *Subscription) RemoveNodes(nodes ...string) error {
	nodeIDs, err := parseNodeSlice(nodes...)
	if err != nil {
		return err
	}
	return s.RemoveNodeIDs(nodeIDs...)
}

// RemoveNodeIDs removes nodes
func (s *Subscription) RemoveNodeIDs(nodes ...*ua.NodeID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	toRemove := make([]uint32, len(nodes))

	for i, node := range nodes {
		sid := node.String()
		handle, ok := s.nodeLookup[sid]
		if !ok {
			return fmt.Errorf("node not found: %s", sid)
		}
		delete(s.nodeLookup, sid)
		delete(s.handles, handle)

		toRemove[i] = handle
	}

	resp, err := s.sub.Unmonitor(toRemove...)
	if err != nil {
		return err
	}

	if resp.ResponseHeader.ServiceResult != ua.StatusOK {
		return resp.ResponseHeader.ServiceResult
	}

	return nil
}

func parseNodeSlice(nodes ...string) ([]*ua.NodeID, error) {
	var err error

	nodeIDs := make([]*ua.NodeID, len(nodes))

	for i, node := range nodes {
		if nodeIDs[i], err = ua.ParseNodeID(node); err != nil {
			return nil, err
		}
	}

	return nodeIDs, nil
}
