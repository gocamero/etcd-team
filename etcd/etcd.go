package etcd

import (
	"crypto/tls"
	"log"
	"net/http"
	"time"

	"github.com/coreos/etcd/config"
)

const (
	raftPrefix = "/raft"

	participantMode int64 = iota
	standbyMode
	stopMode
)

type Server struct {
	config       *config.Config
	id           int64
	pubAddr      string
	raftPubAddr  string
	tickDuration time.Duration

	mode  atomicInt
	nodes map[string]bool
	p     *participant
	s     *standby

	client  *v2client
	peerHub *peerHub

	stopc chan struct{}
}

func New(c *config.Config, id int64) *Server {
	if err := c.Sanitize(); err != nil {
		log.Fatalf("failed sanitizing configuration: %v", err)
	}

	tc := &tls.Config{
		InsecureSkipVerify: true,
	}
	var err error
	if c.PeerTLSInfo().Scheme() == "https" {
		tc, err = c.PeerTLSInfo().ClientConfig()
		if err != nil {
			log.Fatal("failed to create raft transporter tls:", err)
		}
	}

	tr := new(http.Transport)
	tr.TLSClientConfig = tc
	client := &http.Client{Transport: tr}

	s := &Server{
		config:       c,
		id:           id,
		pubAddr:      c.Addr,
		raftPubAddr:  c.Peer.Addr,
		tickDuration: defaultTickDuration,

		mode:  atomicInt(stopMode),
		nodes: make(map[string]bool),

		client:  newClient(tc),
		peerHub: newPeerHub(c.Peers, client),

		stopc: make(chan struct{}),
	}
	for _, seed := range c.Peers {
		s.nodes[seed] = true
	}

	return s
}

func (s *Server) SetTick(tick time.Duration) {
	s.tickDuration = tick
}

// Stop stops the server elegently.
func (s *Server) Stop() {
	if s.mode.Get() == stopMode {
		return
	}
	m := s.mode.Get()
	s.mode.Set(stopMode)
	switch m {
	case participantMode:
		s.p.stop()
	case standbyMode:
		s.s.stop()
	}
	<-s.stopc
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch s.mode.Get() {
	case participantMode, standbyMode:
		s.p.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) RaftHandler() http.Handler {
	return http.HandlerFunc(s.ServeRaftHTTP)
}

func (s *Server) ServeRaftHTTP(w http.ResponseWriter, r *http.Request) {
	switch s.mode.Get() {
	case participantMode:
		s.p.raftHandler().ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) Run() {
	next := participantMode
	for {
		switch next {
		case participantMode:
			s.p = newParticipant(s.id, s.pubAddr, s.raftPubAddr, s.nodes, s.client, s.peerHub, s.tickDuration)
			s.mode.Set(participantMode)
			next = s.p.run()
		case standbyMode:
			s.s = newStandby(s.id, s.pubAddr, s.raftPubAddr, s.nodes, s.client, s.peerHub)
			s.mode.Set(standbyMode)
			next = s.s.run()
		case stopMode:
			s.client.CloseConnections()
			s.peerHub.stop()
			s.stopc <- struct{}{}
			return
		default:
			panic("unsupport mode")
		}
	}
}
