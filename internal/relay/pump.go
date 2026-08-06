package relay

import (
	"errors"
	"fmt"
	"time"

	"github.com/haivision/srtgo"
)

const packetSize = 1316

func (s *Stream) run(host string, latency int) {
	defer close(s.stopped)

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

			// Leg 2: receiver connects to a persistent egress listener.
			outSock, oerr := s.acceptEgress(host, s.OutPort, options, inSock)
			if oerr != nil {
				s.logf("egress accept error on %d: %v", s.OutPort, oerr)
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
			s.logf("egress accepted on %d", s.OutPort)

			// Clear the probe deadline so the pump's reads don't instantly
			// time out (zero time = no deadline).
			inSock.SetReadDeadline(time.Time{})

			// Only treat ingress as the publisher leg; grab its streamid.
			streamID, _ := inSock.GetSockOptString(srtgo.SRTO_STREAMID)

			s.mu.Lock()
			s.State = StateRelaying
			s.ConnectedAt = time.Now()
			s.Codecs = nil
			s.Stats = Stats{Health: HealthGreen}
			s.EgressConnected = true
			s.mu.Unlock()
			s.update()

			// Pump ingress -> egress while sniffing TS for codecs.
			s.pump(inSock, outSock, streamID)

			inSock.Close()
			outSock.Close()

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

// acceptEgress creates a persistent egress listener and accepts one reader.
// It aborts if the stream is stopped/deactivated or if the publisher
// (ingress socket) has gone away while we wait for a reader.
func (s *Stream) acceptEgress(host string, port int, options map[string]string, inSock *srtgo.SrtSocket) (*srtgo.SrtSocket, error) {
	listener, err := s.newListener(host, port, options)
	if err != nil {
		return nil, err
	}
	defer listener.Close()

	probeBuf := make([]byte, 1400)

	for {
		select {
		case <-s.stopCh:
			return nil, fmt.Errorf("stopped")
		default:
		}

		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if !active {
			return nil, fmt.Errorf("deactivated")
		}

		// Probe the publisher socket: a short read forces SRT to notice a
		// lost peer (SockState alone stays CONNECTED because nothing polls
		// the socket during the wait). A deadline timeout means "alive but
		// idle"; any other error means the publisher is gone.
		inSock.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, rerr := inSock.Read(probeBuf)
		if rerr != nil {
			if !isTimeout(rerr) {
				return nil, fmt.Errorf("publisher gone: %v", rerr)
			}
		} else if n > 0 {
			s.touch()
		}
		if inSock.SockState() != srtgo.SRTS_CONNECTED {
			return nil, fmt.Errorf("publisher gone (state %d)", inSock.SockState())
		}

		s.logf("accepting egress on port %d", port)
		listener.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		conn, _, aerr := listener.Accept()
		if aerr != nil {
			select {
			case <-s.stopCh:
				return nil, fmt.Errorf("stopped")
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		conn.SetPollTimeout(1 * time.Second)
		return conn, nil
	}
}

// isTimeout reports whether an srtgo error is a poll/timeout condition (i.e.
// the peer is connected but simply has no data right now).
func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

func (s *Stream) pump(in, out *srtgo.SrtSocket, streamID string) {
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
				Health:        evaluateHealth(bitrate, st, s.Stats.BitrateKbps),
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

		if _, err := out.Write(buf[:n]); err != nil {
			return
		}
		s.mu.Lock()
		s.Stats.BytesOut += int64(n)
		s.mu.Unlock()
	}
}
