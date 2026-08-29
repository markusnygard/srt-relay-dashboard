package relay

// Payload type constants exposed on the stream JSON (payloadType).
const (
	PayloadMPEGTS  = "mpegts"
	PayloadEFP     = "efp"
	PayloadUnknown = "unknown"
)

// efpContentNames maps EFP type-2 fragment hDataContent values (see
// github.com/OwnZones/efp ElasticFrameProtocol.h) to human readable names.
var efpContentNames = map[uint8]string{
	0x00: "unknown",
	0x01: "private",
	0x02: "AAC (ADTS)",
	0x03: "MPEG-TS",
	0x04: "PES",
	0x05: "JPEG 2000",
	0x06: "JPEG",
	0x07: "JPEG XS",
	0x08: "PCM/AES3",
	0x09: "NDI",
	0x0a: "JSON",
	0x80: "EFP Signal",
	0x81: "DID/SDID",
	0x82: "SDI",
	0x83: "H.264",
	0x84: "H.265",
	0x85: "H.266",
	0x86: "AV1",
	0x87: "MP4",
	0x88: "AAC",
	0x89: "Opus",
	0x8a: "FLAC",
}

func efpContentType(cid uint8) string {
	switch cid {
	case 0x82, 0x83, 0x84, 0x85, 0x86, 0x05, 0x06, 0x07:
		return "video"
	case 0x02, 0x08, 0x88, 0x89, 0x8a:
		return "audio"
	default:
		return "other"
	}
}

const snifferMaxMsgs = 80 // classify the payload as unknown after this many messages

// payloadSniffer inspects the SRT messages of a stream to classify the payload
// as MPEG-TS or Elastic Frame Protocol (EFP) and to extract whatever codec
// information each framing exposes.
//
// MPEG-TS: each datagram usually starts with the 0x47 sync byte and the TS
// PAT/PMT reveal the elementary streams (H.264/HEVC/AV1 + audio).
//
// EFP: fragments are validated structurally (frame type in the low nibble of
// the type byte, header sizes and length prefix checked) so random payload
// bytes cannot be mistaken for EFP. EFP is typically sent as one fragment per
// SRT message; some senders prepend a 4-byte big-endian length. The content
// type only appears in type-2 fragments, so the sniffer keeps watching for the
// whole relaying session.
type payloadSniffer struct {
	ts        *tsParser
	tsSyncs   int
	efpFrames int
	msgs      int
	payload   string
	content   map[uint8]Codec

	payloadEmitted bool
	tsEmitted      bool
}

func newPayloadSniffer() *payloadSniffer {
	return &payloadSniffer{
		ts:      newTSParser(),
		content: make(map[uint8]Codec),
	}
}

// tryEFP reports whether data looks like a valid EFP fragment. The EFP header
// may start at offset 0 (reference implementation) or at offset 4 (senders that
// prepend a 4-byte big-endian length, whose high bytes are zero for the small
// message sizes used here). For type-2 fragments it also returns the content
// type byte.
//
// To avoid false positives from random payload bytes (e.g. EFP continuation
// fragments), type-1/3/4 fragments are only accepted when the length prefix is
// present, and type-2 fragments must satisfy the header size fields.
func tryEFP(data []byte) (ft int, content uint8, ok bool) {
	for _, off := range []int{0, 4} {
		if off >= len(data) {
			continue
		}
		if off == 4 && (data[0] != 0 || data[1] != 0) {
			continue
		}
		ft := int(data[off] & 0x0F)
		if ft < 1 || ft > 4 {
			continue
		}
		switch ft {
		case 1, 3, 4:
			// Only accept when prefixed (the reference header-less EFP is
			// still classified via its type-2 tail fragments).
			if off != 4 {
				continue
			}
			if ft != 4 {
				if off+10 > len(data) {
					continue
				}
				// hOfFragmentNo must describe a multi-fragment frame.
				if of := int(data[off+6]) | int(data[off+7])<<8; of < 1 {
					continue
				}
			}
			return ft, 0, true
		case 2:
			if off+27 > len(data) {
				continue
			}
			c := data[off+2]
			if _, known := efpContentNames[c]; !known {
				continue
			}
			// Some senders use the aligned (32-byte) header; others the
			// packed (27-byte) one. Verify hSizeOfData matches.
			if off+32 <= len(data) {
				if sz := int(data[off+4]) | int(data[off+5])<<8; sz == len(data)-off-32 {
					return ft, c, true
				}
			}
			if sz := int(data[off+3]) | int(data[off+4])<<8; sz == len(data)-off-27 {
				return ft, c, true
			}
			continue
		}
	}
	return 0, 0, false
}

// feed inspects one SRT message and reports whether the payload type or the
// detected codecs changed since the last call.
func (p *payloadSniffer) feed(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	p.msgs++

	// Classify the payload type once, from the earliest messages.
	if p.payload == "" {
		if data[0] == 0x47 {
			p.tsSyncs++
		} else if ft, _, ok := tryEFP(data); ok && ft != 0 {
			p.efpFrames++
		}
		switch {
		case p.tsSyncs >= 2 && p.tsSyncs > p.efpFrames:
			p.payload = PayloadMPEGTS
		case p.efpFrames >= 2 && p.efpFrames > p.tsSyncs:
			p.payload = PayloadEFP
		case p.msgs >= snifferMaxMsgs:
			if p.tsSyncs > 0 {
				p.payload = PayloadMPEGTS
			} else {
				p.payload = PayloadUnknown
			}
		}
	}

	changed := false
	if p.payload != "" && !p.payloadEmitted {
		p.payloadEmitted = true
		changed = true
	}

	switch p.payload {
	case PayloadMPEGTS:
		if !p.ts.done() && data[0] == 0x47 {
			p.ts.feed(data)
		}
		if p.ts.done() && !p.tsEmitted {
			p.tsEmitted = true
			changed = true
		}
	case PayloadEFP:
		if ft, c, ok := tryEFP(data); ok && ft == 2 {
			if _, exists := p.content[c]; !exists {
				p.content[c] = Codec{Type: efpContentType(c), Name: efpContentNames[c], PID: 0}
				changed = true
			}
		}
	}

	return changed
}

func (p *payloadSniffer) payloadType() string {
	return p.payload
}

func (p *payloadSniffer) codecs() []Codec {
	if p.payload == PayloadMPEGTS {
		return p.ts.codecs()
	}
	out := make([]Codec, 0, len(p.content))
	seen := make(map[string]bool)
	for _, c := range p.content {
		key := c.Type + ":" + c.Name
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}
