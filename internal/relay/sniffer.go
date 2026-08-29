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

// Sniffing budgets.
const (
	snifferMaxMsgs    = 60 // give up (and label MPEG-TS or unknown) after this many messages
	snifferEfpMinMsgs = 10 // messages of EFP before declaring done
)

// payloadSniffer inspects the first SRT messages of a stream to classify the
// payload as MPEG-TS or Elastic Frame Protocol (EFP) and to extract whatever
// codec information each framing exposes.
//
// MPEG-TS: each datagram usually starts with the 0x47 sync byte and the TS
// PAT/PMT reveal the elementary streams (H.264/HEVC/AV1 + audio).
//
// EFP: the first byte holds the frame type in the low nibble (0..4) with flags
// in the high nibble; type-2 fragments carry the content type at byte 2.
type payloadSniffer struct {
	ts        *tsParser
	tsSyncs   int
	efpFrames int
	msgs      int
	payload   string
	content   map[uint8]Codec
}

func newPayloadSniffer() *payloadSniffer {
	return &payloadSniffer{
		ts:      newTSParser(),
		content: make(map[uint8]Codec),
	}
}

func (p *payloadSniffer) feed(data []byte) {
	if len(data) == 0 || p.done() {
		return
	}
	p.msgs++

	if data[0] == 0x47 {
		p.tsSyncs++
		p.ts.feed(data)
		if p.ts.done() {
			p.payload = PayloadMPEGTS
		}
	} else {
		ft := int(data[0] & 0x0F)
		if ft >= 1 && ft <= 4 {
			p.efpFrames++
			if ft == 2 && len(data) >= 3 {
				cid := data[2]
				if name, ok := efpContentNames[cid]; ok {
					if _, exists := p.content[cid]; !exists {
						p.content[cid] = Codec{Type: efpContentType(cid), Name: name, PID: 0}
					}
				}
			}
		}
	}

	if p.payload == "" {
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
}

// done reports when classification (and codec sniffing) can stop.
func (p *payloadSniffer) done() bool {
	switch p.payload {
	case PayloadMPEGTS:
		// keep parsing TS until the PMT (codecs) is found or the budget ends
		return p.ts.done() || p.msgs >= snifferMaxMsgs
	case PayloadEFP:
		return p.msgs >= snifferEfpMinMsgs
	default:
		return p.msgs >= snifferMaxMsgs
	}
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
