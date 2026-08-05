package relay

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/haivision/srtgo"
)

// StreamState describes the lifecycle of one relay stream.
type StreamState string

const (
	StateStopped   StreamState = "stopped"
	StateWaiting   StreamState = "waiting"
	StateRelaying  StreamState = "relaying"
	StateError     StreamState = "error"
)

// Codec describes a detected elementary stream from MPEG-TS.
type Codec struct {
	Type   string `json:"type"`   // "video" | "audio"
	Name   string `json:"name"`   // e.g. "H.264", "AAC"
	PID    int    `json:"pid"`
}

// Stats is a snapshot of SRT health metrics for one stream.
type Stats struct {
	BitrateKbps   float64 `json:"bitrateKbps"`   // computed from received bytes delta
	BytesIn       int64   `json:"bytesIn"`
	BytesOut      int64   `json:"bytesOut"`
	Retransmitted int64   `json:"retransmitted"` // pktRcvRetrans
	Lost          int64   `json:"lost"`          // pktRcvLoss
	JitterMs      int64   `json:"jitterMs"`      // msRcvTsbPdDelay
	RTTMs         float64 `json:"rttMs"`
	Health        Health  `json:"health"`
}

// Health is the color-coded status.
type Health string

const (
	HealthGreen  Health = "green"
	HealthYellow Health = "yellow"
	HealthRed    Health = "red"
	HealthGray   Health = "gray"
)

// Stream is one configured relay (ingress port -> egress port).
type Stream struct {
	mu sync.Mutex

	ID        string   `json:"id"`
	Name      string   `json:"name"`
	StreamID  string   `json:"streamId"` // label generated for this stream
	InPort    int      `json:"inPort"`   // sender OBS connects here
	OutPort   int      `json:"outPort"`  // receiver OBS connects here
	State     StreamState `json:"state"`
	Codecs    []Codec  `json:"codecs"`
	Stats     Stats    `json:"stats"`
	ConnectedAt time.Time `json:"connectedAt"`

	stopCh   chan struct{}
	stopped  chan struct{}
	onChange func(*Stream)
}

// Relay manages a set of streams.
type Relay struct {
	mu      sync.Mutex
	streams map[string]*Stream
	host    string
	latency int
	onChange func(*Stream)
}

// New creates a relay manager. onChange is invoked on every stream update.
func New(host string, latency int, onChange func(*Stream)) *Relay {
	srtgo.InitSRT()
	r := &Relay{
		streams:  make(map[string]*Stream),
		host:     host,
		latency:  latency,
		onChange: onChange,
	}
	return r
}

// AddStream registers a new relay stream and starts waiting for connections.
func (r *Relay) AddStream(name, streamID string, inPort, outPort int) *Stream {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := streamID
	if id == "" {
		id = name
	}
	s := &Stream{
		ID:        id,
		Name:      name,
		StreamID:  id,
		InPort:    inPort,
		OutPort:   outPort,
		State:     StateStopped,
		stopCh:    make(chan struct{}),
		stopped:   make(chan struct{}),
		onChange:  r.onChange,
	}
	r.streams[id] = s
	go s.run(r.host, r.latency)
	return s
}

// RemoveStream stops and removes a stream.
func (r *Relay) RemoveStream(id string) {
	r.mu.Lock()
	s, ok := r.streams[id]
	if ok {
		delete(r.streams, id)
	}
	r.mu.Unlock()

	if ok {
		close(s.stopCh)
		<-s.stopped
	}
}

// GetStream returns a stream by id.
func (r *Relay) GetStream(id string) *Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[id]
}

// ListStreams returns all streams sorted by ingress port.
func (r *Relay) ListStreams() []*Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Stream, 0, len(r.streams))
	for _, s := range r.streams {
		out = append(out, s)
	}
	return out
}

// PortInUse reports whether ingress or egress port is already assigned.
func (r *Relay) PortInUse(port int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.streams {
		if s.InPort == port || s.OutPort == port {
			return true
		}
	}
	return false
}

// SetOnChange sets the callback invoked whenever a stream updates.
func (r *Relay) SetOnChange(fn func(*Stream)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
	for _, s := range r.streams {
		s.onChange = fn
	}
}

// CleanupSRT shuts down the SRT library.
func CleanupSRT() {
	srtgo.CleanupSRT()
}

func (s *Stream) update() {
	if s.onChange != nil {
		s.onChange(s)
	}
}

func (s *Stream) logf(format string, args ...any) {
	log.Printf("[%s] %s", s.ID, fmt.Sprintf(format, args...))
}

func (s *Stream) setState(st StreamState) {
	s.mu.Lock()
	s.State = st
	s.mu.Unlock()
	s.update()
}

