package relay

import (
	"time"
)

// schedulerLoop periodically checks every stream's schedule, starts/stops the
// accept loop, removes one-off streams that passed their stop time (when
// autoRemove is set), and cleans up idle streams.
func (r *Relay) schedulerLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

func (r *Relay) tick() {
	r.mu.Lock()
	streams := make([]*Stream, 0, len(r.streams))
	for _, s := range r.streams {
		streams = append(streams, s)
	}
	idleMin := r.idleRemoveMin
	r.mu.Unlock()

	now := time.Now()

	for _, s := range streams {
		s.mu.Lock()
		active := s.isActiveNowLocked()

		// One-off auto-remove: stop time passed 15+ min ago, stream idle, and autoRemove set.
		remove := false
		if s.AutoRemove && s.Recurrence == "" && s.StopAt != nil && !now.Before(s.StopAt.Add(15*time.Minute)) {
			if s.State != StateRelaying {
				remove = true
			}
		}

		// Idle cleanup: manual streams (no schedule) with no publisher recently.
		if !remove && idleMin > 0 && s.StartAt == nil && s.StopAt == nil && s.Recurrence == "" {
			if !s.lastActivity.IsZero() && now.Sub(s.lastActivity) > time.Duration(idleMin)*time.Minute {
				remove = true
			}
		}

		changed := active != s.active
		s.active = active
		s.mu.Unlock()

		if changed {
			s.setState(StateScheduled)
		}
		if remove {
			r.RemoveStream(s.ID)
		}
	}
}

// touch marks the stream as having an active publisher.
func (s *Stream) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}
