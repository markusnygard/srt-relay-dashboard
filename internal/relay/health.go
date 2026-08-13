package relay

import "github.com/haivision/srtgo"

// evaluateHealth computes a color-coded health based on packet loss and buffer
// delay. A stream that has never delivered data or has lost the connection
// stays gray/red.
//
// Only *unrecovered* loss is treated as a problem: SRT retransmits lost packets,
// so lost==retransmitted means the link recovered and is healthy. Bitrate is
// deliberately NOT used as a health signal — with a ~2s keyframe interval and
// ~1s sampling, the instantaneous bitrate oscillates wildly (keyframe vs
// non-keyframe windows) and produces false drop alarms.
func evaluateHealth(bitrateKbps float64, st *srtgo.SrtStats, prevBitrate float64, latency int) Health {
	if st == nil {
		return HealthGray
	}

	// No traffic at all -> red.
	if bitrateKbps <= 0 && st.MbpsRecvRate <= 0 {
		return HealthRed
	}

	// Unrecovered packet loss ratio: packets still missing after SRT
	// retransmission. Recovered packets (lost == retransmitted) don't count.
	lost := int64(st.PktRcvLoss)
	retrans := int64(st.PktRcvRetrans)
	unrecovered := lost - retrans
	if unrecovered < 0 {
		unrecovered = 0
	}
	lossRate := float64(unrecovered) / float64(max(st.PktRecv, 1))

	// How far the buffer delay exceeds the configured latency.
	excessDelay := int64(st.MsRcvTsbPdDelay) - int64(latency)

	if lossRate > 0.01 || excessDelay > 1000 {
		return HealthRed
	}
	if lossRate > 0.001 || excessDelay > 300 {
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

