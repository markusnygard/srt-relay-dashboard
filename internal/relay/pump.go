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

		// Accept loop on the same ingress listener until deactivated/stopped.
		for {
			select {
			case <-s.stopCh:
				inListener.Close()
				return
			default:
			}

			s.mu.Lock()
			active = s.active
			s.mu.Unlock()
			if !active {
				inListener.Close()
				break
			}

			s.logf("accepting ingress on port %d", s.InPort)
			inSock, _, err := inListener.Accept()
			if err != nil {
				// transient accept error: keep listening, brief pause
				select {
				case <-s.stopCh:
					inListener.Close()
					return
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			s.logf("ingress accepted on %d", s.InPort)
			s.touch()
			inSock.SetPollTimeout(1 * time.Second)

			s.mu.Lock()
			s.IngressConnected = true
			s.EgressConnected = false
			s.mu.Unlock()
			s.update()

			// Only treat ingress as the publisher leg; grab its streamid.
			streamID, _ := inSock.GetSockOptString(srtgo.SRTO_STREAMID)

			// Persistent egress listener accepts any number of readers and
			// broadcasts every ingress chunk to all of them (fan-out).
			outListener, err := s.newListener(host, s.OutPort, options)
			if err != nil {
				s.logf("egress listen error on %d: %v", s.OutPort, err)
				inSock.Close()
				s.mu.Lock()
				s.IngressConnected = false
				s.mu.Unlock()
				s.update()
				select {
				case <-s.stopCh:
					inListener.Close()
					return
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}

			readers := &egressFanout{socks: make(map[*srtgo.SrtSocket]struct{})}
			stopRead := make(chan struct{})
			readDone := make(chan struct{})
			go s.acceptReaders(outListener, readers, stopRead, readDone)

			s.mu.Lock()
			s.State = StateRelaying
			s.ConnectedAt = time.Now()
			s.Codecs = nil
			s.Stats = Stats{Health: HealthGreen}
			s.mu.Unlock()
			s.update()

			// Pump ingress -> all egress readers while sniffing TS for codecs.
			s.pump(inSock, readers, streamID)

			close(stopRead)
			<-readDone
			outListener.Close()
			readers.closeAll()
			inSock.Close()

			// Reset to waiting so the dashboard no longer shows relaying
			// after the publisher/reader disconnect.
			s.mu.Lock()
			s.State = StateWaiting
			s.Codecs = nil
			s.Stats = Stats{}
			s.IngressConnected = false
			s.EgressConnected = false
			s.mu.Unlock()
			s.update()
		}
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

func (s *Stream) pump(in *srtgo.SrtSocket, readers *egressFanout, streamID string) {
	buf := make([]byte, 1400)
	parser := newTSParser()
	codecsFound := false

	statsTicker := time.NewTicker(1 * time.Second)
	defer statsTicker.Stop()

	lastTime := time.Now()

	for {
		select {
		case <-s.stopCh:
			return
		case <-statsTicker.C:
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

		// Sniff codecs from the first TS packets.
		if !codecsFound {
			parser.feed(buf[:n])
			if parser.done() {
				s.mu.Lock()
				s.Codecs = parser.codecs()
				s.mu.Unlock()
				s.update()
				codecsFound = true
			}
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
