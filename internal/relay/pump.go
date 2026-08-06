package relay

import (
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

		// Leg 1: sender OBS connects to ingress port.
		s.logf("accepting ingress on port %d", s.InPort)
		inSock, err := s.acceptOne(host, s.InPort, options)
		if err != nil {
			s.logf("ingress accept error on %d: %v", s.InPort, err)
			select {
			case <-s.stopCh:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		s.logf("ingress accepted on %d", s.InPort)
		s.touch()

		// Leg 2: receiver OBS connects to egress port.
		s.logf("accepting egress on port %d", s.OutPort)
		outSock, err := s.acceptOne(host, s.OutPort, options)
		if err != nil {
			s.logf("egress accept error on %d: %v", s.OutPort, err)
			inSock.Close()
			continue
		}
		s.logf("egress accepted on %d", s.OutPort)

		// Only treat ingress as the publisher leg; grab its streamid.
		streamID, _ := inSock.GetSockOptString(srtgo.SRTO_STREAMID)

		s.mu.Lock()
		s.State = StateRelaying
		s.ConnectedAt = time.Now()
		s.Codecs = nil
		s.Stats = Stats{Health: HealthGreen}
		s.mu.Unlock()
		s.update()

		// Pump ingress -> egress while sniffing TS for codecs.
		s.pump(inSock, outSock, streamID)

		inSock.Close()
		outSock.Close()

		select {
		case <-s.stopCh:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Stream) acceptOne(host string, port int, options map[string]string) (*srtgo.SrtSocket, error) {
	sock := srtgo.NewSrtSocket(host, uint16(port), options)
	if sock == nil {
		return nil, fmt.Errorf("failed to create srt socket on %d", port)
	}
	if err := sock.Listen(2); err != nil {
		sock.Close()
		return nil, err
	}
	sock.SetPollTimeout(500 * time.Millisecond)
	conn, _, err := sock.Accept()
	sock.Close() // the listening socket is no longer needed
	if err != nil {
		return nil, err
	}
	conn.SetPollTimeout(1 * time.Second)
	return conn, nil
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

