package relay

import (
	"encoding/json"
	"os"
	"time"
)

// persistStream is the on-disk representation of a Stream.
type persistStream struct {
	Name       string     `json:"name"`
	StreamID   string     `json:"streamId"`
	InPort     int        `json:"inPort"`
	OutPort    int        `json:"outPort"`
	StartAt    *time.Time `json:"startAt"`
	StopAt     *time.Time `json:"stopAt"`
	Recurrence string     `json:"recurrence"`
	AutoRemove bool       `json:"autoRemove"`
	Contact    string     `json:"contact"`
}

func (r *Relay) persist() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.persistLocked()
}

// persistLocked writes all streams to the persistence file. Caller holds r.mu.
func (r *Relay) persistLocked() {
	if r.persistFile == "" {
		return
	}
	out := make([]persistStream, 0, len(r.streams))
	for _, s := range r.streams {
		out = append(out, persistStream{
			Name:       s.Name,
			StreamID:   s.StreamID,
			InPort:     s.InPort,
			OutPort:    s.OutPort,
			StartAt:    s.StartAt,
			StopAt:     s.StopAt,
			Recurrence: s.Recurrence,
			AutoRemove: s.AutoRemove,
			Contact:    s.Contact,
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(r.persistFile, data, 0o600)
}

// LoadStreams restores persisted streams. Call after ConfigurePersistence and
// before Start so the scheduler sees them.
func (r *Relay) LoadStreams() error {
	r.mu.Lock()
	file := r.persistFile
	r.mu.Unlock()
	if file == "" {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var ps []persistStream
	if err := json.Unmarshal(data, &ps); err != nil {
		return err
	}
	for _, p := range ps {
		r.AddStream(p.Name, p.StreamID, p.InPort, p.OutPort, p.StartAt, p.StopAt, p.Recurrence, p.AutoRemove, p.Contact)
	}
	return nil
}
