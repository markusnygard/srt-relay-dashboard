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
	StateScheduled StreamState = "scheduled"
	StateWaiting   StreamState = "waiting"
	StateRelaying  StreamState = "relaying"
	StateError     StreamState = "error"
)

// Codec describes a detected elementary stream from MPEG-TS.
type Codec struct {
	Type string `json:"type"` // "video" | "audio"
	Name string `json:"name"` // e.g. "H.264", "AAC"
	PID  int    `json:"pid"`
}

// Stats is a snapshot of SRT health metrics for one stream.
type Stats struct {
	BitrateKbps   float64 `json:"bitrateKbps"`
	BytesIn       int64   `json:"bytesIn"`
	BytesOut      int64   `json:"bytesOut"`
	Retransmitted int64   `json:"retransmitted"`
	Lost          int64   `json:"lost"`
	JitterMs      int64   `json:"jitterMs"`
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

// scheduleStopGrace is the grace period after a stream's StopAt before it is
// deactivated and (if autoRemove is set) removed, so the tail of a broadcast
// is not cut off at the exact scheduled stop time.
const scheduleStopGrace = 1 * time.Minute

// Stream is one configured relay (ingress port -> egress port).
type Stream struct {
	mu sync.Mutex

	ID       string `json:"id"`
	Name     string `json:"name"`
	StreamID string `json:"streamId"` // label generated for this stream
	InPort   int    `json:"inPort"`   // sender OBS connects here
	OutPort  int    `json:"outPort"`  // receiver OBS connects here

	// Scheduling.
	StartAt    *time.Time `json:"startAt"`    // absolute instant; nil = start now
	StopAt     *time.Time `json:"stopAt"`     // absolute instant; nil = manual stop
	Recurrence string     `json:"recurrence"` // "" | "daily" | "weekly"
	AutoRemove bool       `json:"autoRemove"` // remove after stopAt (one-off)
	Contact    string     `json:"contact"`    // free-text contact person

	State            StreamState `json:"state"`
	PayloadType      string      `json:"payloadType"` // "mpegts" | "efp" | "unknown" | ""
	Codecs           []Codec     `json:"codecs"`
	Stats            Stats       `json:"stats"`
	ConnectedAt      time.Time   `json:"connectedAt"`
	IngressConnected bool        `json:"ingressConnected"` // publisher connected
	EgressConnected  bool        `json:"egressConnected"`  // reader connected

	// internal
	r            *Relay
	active       bool
	lastActivity time.Time
	latency      int

	// persistent egress (readers) side, open while the stream is active
	egressListener *srtgo.SrtSocket
	egressReaders  *egressFanout
	egressStop     chan struct{}
	egressDone     chan struct{}
	egressOpen     bool

	stopCh   chan struct{}
	stopped  chan struct{}
	onChange func(*Stream)
}

// Relay manages a set of streams.
type Relay struct {
	mu       sync.Mutex
	streams  map[string]*Stream
	host     string
	latency  int
	onChange func(*Stream)

	persistFile   string
	idleRemoveMin int

	onRemove func(id string)
	stopCh   chan struct{}
}

// New creates a relay manager. onChange is invoked on every stream update.
func New(host string, latency int, onChange func(*Stream)) *Relay {
	srtgo.InitSRT()
	r := &Relay{
		streams:  make(map[string]*Stream),
		host:     host,
		latency:  latency,
		onChange: onChange,
		stopCh:   make(chan struct{}),
	}
	return r
}

// Start starts the scheduler loop. Call after loading persisted streams.
func (r *Relay) Start() {
	go r.schedulerLoop()
}

// Stop shuts down the scheduler and all streams.
func (r *Relay) Stop() {
	close(r.stopCh)
	r.mu.Lock()
	streams := make([]*Stream, 0, len(r.streams))
	for _, s := range r.streams {
		streams = append(streams, s)
	}
	r.streams = map[string]*Stream{}
	r.mu.Unlock()
	for _, s := range streams {
		close(s.stopCh)
		<-s.stopped
	}
}

// ConfigurePersistence sets the streams file and idle-remove threshold.
func (r *Relay) ConfigurePersistence(file string, idleRemoveMin int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.persistFile = file
	r.idleRemoveMin = idleRemoveMin
}

// PersistFile returns the configured streams file path.
func (r *Relay) PersistFile() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.persistFile
}

// IdleRemoveMin returns the idle-remove threshold (0 = disabled).
func (r *Relay) IdleRemoveMin() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.idleRemoveMin
}

// AddStream registers a new relay stream and starts its run loop.
func (r *Relay) AddStream(name, streamID string, inPort, outPort int, startAt, stopAt *time.Time, recurrence string, autoRemove bool, contact string) *Stream {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := streamID
	if id == "" {
		id = name
	}
	s := &Stream{
		ID:         id,
		Name:       name,
		StreamID:   id,
		InPort:     inPort,
		OutPort:    outPort,
		StartAt:    startAt,
		StopAt:     stopAt,
		Recurrence: recurrence,
		AutoRemove: autoRemove,
		Contact:    contact,
		State:      StateStopped,
		stopCh:     make(chan struct{}),
		stopped:    make(chan struct{}),
		onChange:   r.onChange,
		r:          r,
	}
	s.active = s.isActiveNowLocked()
	r.streams[id] = s
	go s.run(r.host, r.latency)
	r.persistLocked()
	return s
}

// PersistNow writes all streams to the persistence file (no-op if none set).
func (r *Relay) PersistNow() {
	r.persist()
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
		r.persist()
		if r.onRemove != nil {
			r.onRemove(id)
		}
	}
}

// GetStream returns a stream by id.
func (r *Relay) GetStream(id string) *Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[id]
}

// findByStreamID returns the stream whose streamId or name matches sid. This is
// used to route ingress connections by the sender's SRT streamid.
func (r *Relay) findByStreamID(sid string) *Stream {
	if sid == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.streams[sid]; s != nil {
		return s
	}
	for _, s := range r.streams {
		if s.Name == sid {
			return s
		}
	}
	return nil
}

// ListStreams returns all streams.
func (r *Relay) ListStreams() []*Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Stream, 0, len(r.streams))
	for _, s := range r.streams {
		out = append(out, s)
	}
	return out
}

// PortClaimed reports whether a port is assigned to any stream (live or scheduled).
func (r *Relay) PortClaimed(port int) bool {
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

// SetOnRemove sets the callback invoked whenever a stream is removed.
func (r *Relay) SetOnRemove(fn func(id string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRemove = fn
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

// UpdateSchedule applies new schedule fields and refreshes the active flag.
func (s *Stream) UpdateSchedule(startAt, stopAt *time.Time, recurrence string, autoRemove bool, contact string) {
	s.mu.Lock()
	s.StartAt = startAt
	s.StopAt = stopAt
	s.Recurrence = recurrence
	s.AutoRemove = autoRemove
	s.Contact = contact
	active := s.isActiveNowLocked()
	changed := active != s.active
	s.active = active
	s.mu.Unlock()
	if changed {
		s.setState(StateScheduled)
	}
	s.update()
}

// isActiveNowLocked reports whether the stream should be accepting connections now.
// Caller must hold s.mu.
func (s *Stream) isActiveNowLocked() bool {
	now := time.Now()
	if s.Recurrence == "" {
		if s.StartAt != nil && now.Before(*s.StartAt) {
			return false
		}
		if s.StopAt != nil && !now.Before(s.StopAt.Add(scheduleStopGrace)) {
			return false
		}
		return true
	}
	// recurring (daily/weekly): anchored to server-local wall clock
	var startB, stopB time.Time
	if s.StartAt != nil {
		startB = latestOccurrence(*s.StartAt, s.Recurrence == "weekly", time.Local, now)
	} else {
		startB = now
	}
	if s.StopAt != nil {
		stopB = latestOccurrence(*s.StopAt, s.Recurrence == "weekly", time.Local, now)
	}
	return startB.After(stopB.Add(scheduleStopGrace))
}

// latestOccurrence returns the most recent time matching base's clock time
// (daily) or weekday+clock time (weekly) at or before T, in the given location.
func latestOccurrence(base time.Time, weekly bool, loc *time.Location, t time.Time) time.Time {
	b := base.In(loc)
	var cand time.Time
	if weekly {
		delta := (int(t.Weekday()) - int(b.Weekday()) + 7) % 7
		cand = time.Date(t.Year(), t.Month(), t.Day(), b.Hour(), b.Minute(), b.Second(), 0, loc).AddDate(0, 0, -delta)
		if cand.After(t) {
			cand = cand.AddDate(0, 0, -7)
		}
	} else {
		cand = time.Date(t.Year(), t.Month(), t.Day(), b.Hour(), b.Minute(), b.Second(), 0, loc)
		if cand.After(t) {
			cand = cand.AddDate(0, 0, -1)
		}
	}
	return cand
}
