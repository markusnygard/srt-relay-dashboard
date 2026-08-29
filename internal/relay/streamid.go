package relay

import "strings"

// cleanStreamID extracts the stream name from a sender's SRT streamid. The
// dashboard URLs use "publish:<name>" for senders and "read:<name>" for
// readers, but a bare "<name>" is also accepted.
func cleanStreamID(raw string) string {
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		return raw[i+1:]
	}
	return raw
}
