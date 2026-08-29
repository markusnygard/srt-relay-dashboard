package relay

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/haivision/srtgo"
)

const packetSize = 1316

func (s *Stream) run(host string, latency int) {
	defer close(s.stopped)

	s.latency = latency

	options := map[string]string{
		"transtype": "live",
		"mode":      "listener",
		"latency":   fmt.Sprintf("%d", latency),
	}

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		// Wait until the schedule says this stream is active.
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if !active {
			s.closeEgress()
			s.setState(StateScheduled)
			select {
			case <-s.stopCh:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		s.setState(StateWaiting)

		// Create one persistent ingress listener that stays bound so SRT
		// handshakes are always received (no 2s gaps between attempts).
		inListener, err := s.newListener(host, s.InPort, options)
		if err != nil {
			s.logf("ingress listen error on %d: %v", s.InPort, err)
			select {
			case <-s.stopCh:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// Persistent egress listener: readers may connect at any time while
		// the stream is active, independent of any single sender session.
		if !s.openEgress(host, options) {
			inListener.Close()
			select {
			case <-s.stopCh:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// Accept loop on the same ingress listener until deactivated/stopped.
		for {
			select {
			case <-s.stopCh:
				inListener.Close()
				s.closeEgress()
				return
			default:
			}

			s.mu.Lock()
			active = s.active
			s.mu.Unlock()
			if !active {
				inListener.Close()
				s.closeEgress()
				break
			}

			s.logf("accepting ingress on port %d", s.InPort)
			inListener.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			inSock, _, err := inListener.Accept()
			if err != nil {
				if isTimeout(err) {
					continue
				}
				// transient accept error: keep listening, brief pause
				select {
				case <-s.stopCh:
					inListener.Close()
					s.closeEgress()
					return
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			s.logf("ingress accepted on %d", s.InPort)
			s.touch()
			inSock.SetPollTimeout(1 * time.Second)

			// Route by the sender's streamid to the matching stream, or
			// reject the sender if no stream matches.
			streamID, _ := inSock.GetSockOptString(srtgo.SRTO_STREAMID)
			s.handleIngress(inSock, streamID)
		}
	}
}

// handleIngress accepts one sender and routes it to the stream whose
// streamId/name matches the sender's streamid, relaying to that stream's
// egress port regardless of which ingress port the sender connected to.
// Senders whose streamid matches no stream are rejected. A missing streamid
// falls back to the port-owning stream.
func (s *Stream) handleIngress(inSock *srtgo.SrtSocket, streamID string) {
	name := cleanStreamID(streamID)
	target := s
	if name != "" {
		target = s.r.findByStreamID(name)
		if target == nil {
			s.logf("rejecting ingress: streamid %q matches no stream", streamID)
			inSock.Close()
			return
		}
	}
	fanout, ok := target.egress()
	if !ok {
		s.logf("rejecting ingress for %q: target stream not active", name)
		inSock.Close()
		return
	}
	target.startRelay(inSock, fanout)
}

// startRelay marks the target stream relaying and pumps the ingress socket to
// its egress fanout until the sender disconnects.
func (t *Stream) startRelay(inSock *srtgo.SrtSocket, fanout *egressFanout) {
	t.mu.Lock()
	t.State = StateRelaying
	t.ConnectedAt = time.Now()
	t.IngressConnected = true
	t.EgressConnected = fanout.count() > 0
	t.Codecs = nil
	t.PayloadType = ""
	t.Stats = Stats{Health: HealthGreen}
	t.mu.Unlock()
	t.update()

	t.pump(inSock, fanout)
	inSock.Close()

	// Reset so the dashboard no longer shows relaying after the publisher
	// disconnects.
	t.mu.Lock()
	t.State = StateWaiting
	t.Codecs = nil
	t.PayloadType = ""
	t.Stats = Stats{}
	t.IngressConnected = false
	t.EgressConnected = fanout.count() > 0
	t.mu.Unlock()
	t.update()
}

// egress returns the stream's persistent egress fanout if open.
func (s *Stream) egress() (*egressFanout, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.egressOpen || s.egressReaders == nil {
		return nil, false
	}
	return s.egressReaders, true
}

// openEgress creates the stream's persistent egress listener and starts
// accepting readers until the stream deactivates or stops.
func (s *Stream) openEgress(host string, options map[string]string) bool {
	ls, err := s.newListener(host, s.OutPort, options)
	if err != nil {
		s.logf("egress listen error on %d: %v", s.OutPort, err)
		return false
	}
	readers := &egressFanout{socks: make(map[*srtgo.SrtSocket]struct{})}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.mu.Lock()
	s.egressListener = ls
	s.egressReaders = readers
	s.egressStop = stop
	s.egressDone = done
	s.egressOpen = true
	s.mu.Unlock()
	go s.acceptReaders(ls, readers, stop, done)
	return true
}

// closeEgress tears down the persistent egress listener and fanout.
func (s *Stream) closeEgress() {
	s.mu.Lock()
	ls := s.egressListener
	readers := s.egressReaders
	stop := s.egressStop
	done := s.egressDone
	open := s.egressOpen
	s.egressOpen = false
	s.egressListener = nil
	s.egressReaders = nil
	s.mu.Unlock()
	if !open {
		return
	}
	readers.closeAll()
	if stop != nil {
		close(stop)
	}
	if ls != nil {
		ls.Close()
	}
	if done != nil {
		<-done
	}
}

// newListener creates and binds a persistent SRT listener socket.
func (s *Stream) newListener(host string, port int, options map[string]string) (*srtgo.SrtSocket, error) {
	sock := srtgo.NewSrtSocket(host, uint16(port), options)
	if sock == nil {
		return nil, fmt.Errorf("failed to create srt socket on %d", port)
	}
	if err := sock.Listen(8); err != nil {
		sock.Close()
		return nil, err
	}
	sock.SetPollTimeout(1 * time.Second)
	return sock, nil
}

// egressFanout tracks every connected reader and broadcasts to them all.
type egressFanout struct {
	mu    sync.Mutex
	socks map[*srtgo.SrtSocket]struct{}
}

func (e *egressFanout) add(s *srtgo.SrtSocket) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.socks[s] = struct{}{}
}

// write sends b to every reader, dropping any that have gone away.
// Returns the number of bytes written to readers.
func (e *egressFanout) write(b []byte) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var written int64
	for s := range e.socks {
		if _, err := s.Write(b); err != nil {
			s.Close()
			delete(e.socks, s)
			continue
		}
		written += int64(len(b))
	}
	return written
}

func (e *egressFanout) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.socks)
}

func (e *egressFanout) closeAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for s := range e.socks {
		s.Close()
	}
	e.socks = make(map[*srtgo.SrtSocket]struct{})
}

// acceptReaders accepts any number of readers on the egress port until the
// stream stops or the pump tears down (publisher gone). It does NOT read
// the ingress socket — the pump handles liveness detection exclusively.
func (s *Stream) acceptReaders(listener *srtgo.SrtSocket, readers *egressFanout, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	for {
		select {
		case <-stop:
			return
		case <-s.stopCh:
			return
		default:
		}

		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if !active {
			return
		}

		s.logf("accepting egress on port %d", s.OutPort)
		listener.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		conn, _, aerr := listener.Accept()
		if aerr != nil {
			if isTimeout(aerr) {
				continue
			}
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		conn.SetPollTimeout(1 * time.Second)
		readers.add(conn)
		s.mu.Lock()
		s.EgressConnected = true
		s.mu.Unlock()
		s.update()
		s.logf("egress accepted on %d (readers=%d)", s.OutPort, readers.count())
	}
}

// isTimeout reports whether an srtgo error is a poll/timeout condition (i.e.
// the peer is connected but simply has no data right now).
func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

func (s *Stream) pump(in *srtgo.SrtSocket, readers *egressFanout) {
	buf := make([]byte, 1400)
	sniffer := newPayloadSniffer()

	statsTicker := time.NewTicker(1 * time.Second)
	defer statsTicker.Stop()

	lastTime := time.Now()

	for {
		select {
		case <-s.stopCh:
			return
		case <-statsTicker.C:
			s.mu.Lock()
			active := s.active
			s.mu.Unlock()
			if !active {
				return
			}

			st, err := in.Stats()
			if err != nil {
				continue
			}

			// srtgo's Stats() calls srt_bstats with clear=1, so ByteRecv is
			// already the number of bytes received since the previous call.
			now := time.Now()
			elapsed := now.Sub(lastTime).Seconds()
			bitrate := 0.0
			if elapsed > 0 {
				bitrate = float64(st.ByteRecv*8) / elapsed / 1000.0 // kbps
			}
			lastTime = now

			s.mu.Lock()
			s.Stats = Stats{
				BitrateKbps:   bitrate,
				BytesIn:       st.ByteRecv,
				Retransmitted: int64(st.PktRcvRetrans),
				Lost:          int64(st.PktRcvLoss),
				JitterMs:      int64(st.MsRcvTsbPdDelay),
				RTTMs:         st.MsRTT,
				Health:        evaluateHealth(bitrate, st, s.Stats.BitrateKbps, s.latency),
			}
			s.mu.Unlock()
			s.update()
		default:
		}

		n, err := in.Read(buf)
		if err != nil {
			return
		}
		if n <= 0 {
			continue
		}
		s.touch()

		// Sniff payload type + codecs; keeps watching for late EFP content.
		if sniffer.feed(buf[:n]) {
			s.mu.Lock()
			s.PayloadType = sniffer.payloadType()
			s.Codecs = sniffer.codecs()
			s.mu.Unlock()
			s.update()
		}

		written := readers.write(buf[:n])
		s.mu.Lock()
		s.Stats.BytesOut += written
		if readers.count() == 0 {
			s.EgressConnected = false
		}
		s.mu.Unlock()
	}
}
