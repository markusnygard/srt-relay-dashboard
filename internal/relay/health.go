package relay

import "github.com/haivision/srtgo"

// evaluateHealth computes a color-coded health based on bitrate stability,
// packet loss and buffer delay. A stream that has never delivered data or has
// lost the connection stays gray/red.
//
// MsRcvTsbPdDelay (the TSBPD buffer delay) is normally ~= the configured SRT
// latency, so it is only treated as a problem when it significantly exceeds
// that baseline — a sign the receiver buffer is growing (congestion), not a
// healthy link.
func evaluateHealth(bitrateKbps float64, st *srtgo.SrtStats, prevBitrate float64, latency int) Health {
	if st == nil {
		return HealthGray
	}

	// No traffic at all -> red.
	if bitrateKbps <= 0 && st.MbpsRecvRate <= 0 {
		return HealthRed
	}

	// Packet loss ratio over the last interval.
	lossRate := float64(st.PktRcvLoss) / float64(max(st.PktRecv, 1))
	retransRate := float64(st.PktRcvRetrans) / float64(max(st.PktRecv, 1))

	// How far the buffer delay exceeds the configured latency.
	excessDelay := int64(st.MsRcvTsbPdDelay) - int64(latency)

	// Big sudden drop vs the previous sample.
	drop := 0.0
	if prevBitrate > 0 {
		drop = (prevBitrate - bitrateKbps) / prevBitrate
	}

	if lossRate > 0.01 || retransRate > 0.02 || excessDelay > 1000 || drop > 0.8 {
		return HealthRed
	}
	if lossRate > 0.001 || retransRate > 0.005 || excessDelay > 300 || drop > 0.4 {
		return HealthYellow
	}
	return HealthGreen
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

