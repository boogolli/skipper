package swarm

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/memberlist"
	log "github.com/sirupsen/logrus"
)

type swarmType int

const (
	swarmKubernetes swarmType = iota
	swarmUnknown
)

func (st swarmType) String() string {
	switch st {
	case swarmKubernetes:
		return "kubernetes Swarm"
	}
	return "unkwown Swarm"
}

func getSwarmType(o Options) swarmType {
	if o.KubernetesOptions.KubernetesInCluster && o.KubernetesOptions.KubernetesAPIBaseURL != "" {
		return swarmKubernetes
	}
	return swarmUnknown
}

const (
	// DefaultMaxMessageBuffer is the default maximum size of the
	// exchange packets send out to peers.
	DefaultMaxMessageBuffer = 1 << 22
	// DefaultPort is used as default to connect to other
	// known swarm peers.
	DefaultPort = 9990
	// DefaultLeaveTimeout is the timeout to wait for responses
	// for a leave message send by this instance to other peers.
	DefaultLeaveTimeout = time.Duration(5 * time.Second)
)

var (
	ErrUnknownSwarm = errors.New("unknown swarm type")
)

// KubernetesOptions specific swarm options
type KubernetesOptions struct {
	KubernetesInCluster  bool
	KubernetesAPIBaseURL string
	Namespace            string
	LabelSelectorKey     string
	LabelSelectorValue   string
}

// Options for swarm objects.
type Options struct {
	swarm swarmType
	// leaky, expected to be buffered, or errors are lost
	Errors            chan<- error // TODO(sszuecs): should probably be hidden as implemetnation detail
	MaxMessageBuffer  int
	LeaveTimeout      time.Duration
	SwarmPort         int
	KubernetesOptions *KubernetesOptions
}

// Swarm is the main type for exchanging low latency, weakly
// consistent information.
type Swarm struct {
	local            *NodeInfo
	errors           chan<- error
	maxMessageBuffer int
	leaveTimeout     time.Duration

	getOutgoing <-chan reqOutgoing
	outgoing    chan *outgoingMessage
	incoming    <-chan []byte
	listeners   map[string]chan<- *Message
	leave       chan struct{}
	getValues   chan *valueReq

	messages [][]byte
	shared   sharedValues
	mlist    *memberlist.Memberlist
}

func NewSwarm(o Options) (*Swarm, error) {
	switch getSwarmType(o) {
	case swarmKubernetes:
		return newKubernetesSwarm(o)
	default:
		return nil, ErrUnknownSwarm
	}
}

func newKubernetesSwarm(o Options) (*Swarm, error) {
	u, err := buildAPIURL(o.KubernetesOptions.KubernetesInCluster, o.KubernetesOptions.KubernetesAPIBaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubernetes API url from url %s running in cluster %v: %v", o.KubernetesOptions.KubernetesAPIBaseURL, o.KubernetesOptions.KubernetesInCluster, err)
	}

	o.Errors = make(chan<- error) // FIXME - do WE have to read this, or...?
	o.KubernetesOptions.KubernetesAPIBaseURL = u

	if o.SwarmPort <= 0 || o.SwarmPort >= 65535 {
		log.Errorf("Wrong SwarmPort %d, set to default %d instead", o.SwarmPort, DefaultMaxMessageBuffer)
		o.SwarmPort = DefaultPort
	}

	if o.KubernetesOptions.Namespace == "" {
		log.Errorf("Namespace is empty set to default %s instead", DefaultNamespace)
		o.KubernetesOptions.Namespace = DefaultNamespace
	}

	if o.KubernetesOptions.LabelSelectorKey == "" {
		log.Errorf("LabelSelectorKey is empty, set to default %s instead", DefaultLabelSelectorKey)
		o.KubernetesOptions.LabelSelectorKey = DefaultLabelSelectorKey
	}

	if o.KubernetesOptions.LabelSelectorValue == "" {
		log.Errorf("LabelSelectorValue is empty, set to default %s instead", DefaultLabelSelectorValue)
		o.KubernetesOptions.LabelSelectorValue = DefaultLabelSelectorValue
	}

	if o.MaxMessageBuffer <= 0 {
		log.Errorf("MaxMessageBuffer <= 0, setting to default %d instead", DefaultMaxMessageBuffer)
		o.MaxMessageBuffer = DefaultMaxMessageBuffer
	}

	if o.LeaveTimeout <= 0 {
		log.Errorf("LeaveTimeout <= 0, setting to default %d instead", DefaultLeaveTimeout)
		o.LeaveTimeout = DefaultLeaveTimeout
	}

	return Start(o)
}

// Start will find Swarm peers and join them.
func Start(o Options) (*Swarm, error) {
	knownEntryPoint := newKnownEntryPoint(o)
	return Join(o, knownEntryPoint.Node(), knownEntryPoint.Nodes())
}

// Join will join given Swarm peers and return an initialiazed Swarm
// object if successful.
// TODO(sszuecs): check the options elsewhere
func Join(o Options, self *NodeInfo, nodes []*NodeInfo) (*Swarm, error) {
	log.Infof("SWARM: Going to join swarm of %d nodes, self=%s", len(nodes), self)
	c := memberlist.DefaultLocalConfig()

	if self.Name == "" {
		self.Name = c.Name
	} else {
		c.Name = self.Name
	}

	if self.Addr == nil {
		self.Addr = net.ParseIP(c.BindAddr)
	} else {
		c.BindAddr = self.Addr.String()
		c.AdvertiseAddr = c.BindAddr
	}
	if self.Port == 0 {
		self.Port = c.BindPort
	} else {
		c.BindPort = self.Port
		c.AdvertisePort = c.BindPort
	}

	getOutgoing := make(chan reqOutgoing)
	outgoing := make(chan *outgoingMessage)
	incoming := make(chan []byte)
	getValues := make(chan *valueReq)
	listeners := make(map[string]chan<- *Message)
	leave := make(chan struct{})
	shared := make(sharedValues)

	c.Delegate = &mlDelegate{
		outgoing: getOutgoing,
		incoming: incoming,
	}

	ml, err := memberlist.Create(c)
	if err != nil {
		log.Errorf("SWARM: failed to create memberlist: %v", err)
		return nil, err
	}

	c.Delegate.(*mlDelegate).meta = ml.LocalNode().Meta

	if len(nodes) > 0 {
		addresses := mapNodesToAddresses(nodes)
		_, err := ml.Join(addresses)
		if err != nil {
			log.Errorf("SWARM: failed to join: %v", err)
			return nil, err
		}
	}

	s := &Swarm{
		local:            self,
		errors:           o.Errors,
		maxMessageBuffer: o.MaxMessageBuffer,
		leaveTimeout:     o.LeaveTimeout,
		getOutgoing:      getOutgoing,
		outgoing:         outgoing,
		incoming:         incoming,
		getValues:        getValues,
		listeners:        listeners,
		leave:            leave,
		shared:           shared,
	}

	// TODO(sszuecs): maybe we should wrap it in a recover for panic, but we need to close the channels
	go s.control()

	return s, nil
}

// control is the control loop of a Swarm member.
func (s *Swarm) control() {
	for {
		// TODO: regularly check the available instances <- Do we need this?

		select {
		case req := <-s.getOutgoing:
			s.messages = takeMaxLatest(s.messages, req.overhead, req.limit)
			if len(s.messages) > 0 {
				log.Infof("SWARM: getOutgoing %d messages", len(s.messages))
			} else {
				// XXX(sszuecs): does this happen?
				log.Debug("SWARM: getOutgoing with 0 messages")
			}
			req.ret <- s.messages
		case m := <-s.outgoing:
			s.messages = append(s.messages, m.encoded)
			s.messages = takeMaxLatest(s.messages, 0, s.maxMessageBuffer)
			log.Infof("SWARM: outgoing %d messages", len(s.messages))
			if m.message.Type == sharedValue {
				log.Infof("SWARM: share value: %s %s: %v", s.local.Name, m.message.Key, m.message.Value)
				s.shared.set(s.local.Name, m.message.Key, m.message.Value)
			}
		case b := <-s.incoming:
			m, err := decodeMessage(b)
			if err != nil {
				// assuming buffered error channels
				select {
				case s.errors <- err:
				default:
				}
			} else if m.Type == sharedValue {
				log.Infof("SWARM: got shared value: %s %s: %v", m.Source, m.Key, m.Value)
				s.shared.set(m.Source, m.Key, m.Value)
			} else if m.Type == broadcast {
				log.Infof("SWARM: got broadcast value: %s %s: %v", m.Source, m.Key, m.Value)
				for k, l := range s.listeners {
					if k == m.Key {
						// assuming buffered listener channels
						select {
						case l <- &Message{
							Source: m.Source,
							Value:  m.Value,
						}:
						default:
						}
					}
				}
			}
		case req := <-s.getValues:
			log.Infof("SWARM: getValues for key: %s", req.key)
			req.ret <- s.shared[req.key]
		case <-s.leave:
			log.Infof("SWARM: leaving %s", s.local)
			// TODO: call shutdown
			if err := s.mlist.Leave(s.leaveTimeout); err != nil {
				select {
				case s.errors <- err:
				default:
				}
			}

			return
		}
	}
}

// Local is a getter to the local member of a swarm.
// TODO: memberlist has support for this, less redundant to use that
func (s *Swarm) Local() *NodeInfo { return s.local }

func (s *Swarm) broadcast(m *message) error {
	m.Source = s.Local().Name
	b, err := encodeMessage(m)
	if err != nil {
		return err
	}

	s.outgoing <- &outgoingMessage{
		message: m,
		encoded: b,
	}
	return nil
}

// Broadcast sends a broadcast message with a value to all peers.
func (s *Swarm) Broadcast(m interface{}) error {
	return s.broadcast(&message{Type: broadcast, Value: m})
}

// ShareValue sends a broadcast message with a sharedValue to all peers.
func (s *Swarm) ShareValue(key string, value interface{}) error {
	return s.broadcast(&message{Type: sharedValue, Key: key, Value: value})
}

// DeleteValue does nothing, but implements an interface.
func (s *Swarm) DeleteValue(string) error { return nil }

// Values implements an interface to send a request and wait blocking
// for a response.
func (s *Swarm) Values(key string) map[string]interface{} {
	req := &valueReq{
		key: key,
		ret: make(chan map[string]interface{}),
	}
	s.getValues <- req
	return <-req.ret
}

// XXX(sszuecs): required? seems not
// func (s *Swarm) Members() []*NodeInfo            { return nil }
// func (s *Swarm) State() NodeState                { return Initial }
// func (s *Swarm) Instances() map[string]NodeState { return nil }

// assumed to buffered or may drop
func (s *Swarm) Listen(key string, c chan<- *Message) {}

func (s *Swarm) Leave() {
	close(s.leave)
}