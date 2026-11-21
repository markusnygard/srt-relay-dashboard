package relay

import "strings"

// cleanStreamID extracts the stream name from a sender's SRT streamid.
// Accepted forms:
//
//	<name>
//	publish:<name>            (what the dashboard shows; "read:" likewise)
//	publish:<name>:<user>:<pass>[:...]  (MediaMTX style; extra parts ignored,
//	                                     so one encoder config works for both)
//	#!::r=<name>,m=publish,...          (SRT access-control syntax)
func cleanStreamID(raw string) string {
	if strings.HasPrefix(raw, "#!::") {
		for _, kv := range strings.Split(raw[4:], ",") {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.TrimSpace(k) == "r" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	if _, rest, ok := strings.Cut(raw, ":"); ok {
		name, _, _ := strings.Cut(rest, ":")
		return name
	}
	return raw
}
