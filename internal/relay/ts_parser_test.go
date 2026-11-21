package relay

import (
	"fmt"
	"strings"
	"testing"
)

// --- helpers to build synthetic MPEG-TS ---

type esDef struct {
	streamType int
	pid        int
	desc       []byte
}

func desc(tag byte, data ...byte) []byte { return append([]byte{tag, byte(len(data))}, data...) }
func lang(l string) []byte               { return desc(descISO639, l[0], l[1], l[2], 0) }
func reg(fourcc string) []byte {
	return desc(descRegistration, fourcc[0], fourcc[1], fourcc[2], fourcc[3])
}

// withCRC finishes a section: fills section_length and appends a dummy CRC.
func withCRC(sec []byte) []byte {
	sec = append(sec, 0, 0, 0, 0)
	l := len(sec) - 3
	sec[1] = 0xB0 | byte(l>>8)
	sec[2] = byte(l)
	return sec
}

func patSection(pmtPIDs ...int) []byte {
	sec := []byte{0x00, 0, 0, 0x00, 0x01, 0xC1, 0x00, 0x00}
	for i, pid := range pmtPIDs {
		sec = append(sec, 0, byte(i+1), 0xE0|byte(pid>>8), byte(pid))
	}
	return withCRC(sec)
}

func pmtSection(progInfo []byte, es ...esDef) []byte {
	sec := []byte{0x02, 0, 0, 0x00, 0x01, 0xC1, 0x00, 0x00, 0xE1, 0x00,
		0xF0 | byte(len(progInfo)>>8), byte(len(progInfo))}
	sec = append(sec, progInfo...)
	for _, e := range es {
		sec = append(sec, byte(e.streamType), 0xE0|byte(e.pid>>8), byte(e.pid),
			0xF0|byte(len(e.desc)>>8), byte(len(e.desc)))
		sec = append(sec, e.desc...)
	}
	return withCRC(sec)
}

// packets splits a section into TS packets on pid. pointer bytes of junk are
// placed before the section start (to exercise pointer_field), and the first
// packet optionally carries an adaptation field.
func packets(pid int, section []byte, pointer int, adaptation bool) []byte {
	payload := append([]byte{byte(pointer)}, make([]byte, pointer)...)
	payload = append(payload, section...)
	var out []byte
	first := true
	for len(payload) > 0 {
		pkt := make([]byte, tsPacketSize)
		for i := range pkt {
			pkt[i] = 0xFF
		}
		pkt[0] = tsSyncByte
		pkt[1] = byte(pid>>8) & 0x1F
		if first {
			pkt[1] |= 0x40
		}
		pkt[2] = byte(pid)
		pos := 4
		if first && adaptation {
			pkt[3] = 0x30
			pkt[4] = 7 // adaptation_field_length
			pkt[5] = 0x00
			pos = 4 + 1 + 7
		} else {
			pkt[3] = 0x10
		}
		n := copy(pkt[pos:], payload)
		payload = payload[n:]
		out = append(out, pkt...)
		first = false
	}
	return out
}

func describe(cs []Codec) string {
	var parts []string
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s:%s:%d:%s", c.Type, c.Name, c.PID, c.Lang))
	}
	return strings.Join(parts, " | ")
}

// --- tests ---

func TestSimpleProgram(t *testing.T) {
	p := newTSParser()
	ts := packets(0, patSection(0x100), 0, false)
	ts = append(ts, packets(0x100, pmtSection(nil,
		esDef{0x1B, 0x101, nil},
		esDef{0x0F, 0x102, nil},
	), 0, false)...)
	p.feed(ts)
	got := describe(p.codecs())
	want := "video:H.264:257: | audio:AAC (ADTS):258:"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !p.done() {
		t.Fatal("done() should be true")
	}
}

// A PMT with many tracks spans several TS packets; every track must be seen,
// same-codec audio tracks must not be merged, and 0x06 streams must be
// classified from their descriptors.
func TestMultiPacketPMTAllTracks(t *testing.T) {
	big := desc(0xF0, make([]byte, 150)...) // forces the PMT over one packet
	pmt := pmtSection(nil,
		esDef{0x1B, 0x100, big},
		esDef{0x0F, 0x101, lang("fin")},
		esDef{0x0F, 0x102, lang("swe")},
		esDef{0x0F, 0x103, lang("eng")},
		esDef{0x0F, 0x104, lang("qaa")},
		esDef{0x06, 0x105, append(desc(descAC3, 0x00), lang("eng")...)},
		esDef{0x06, 0x106, desc(descSubtitling, 'f', 'i', 'n', 0x10, 0, 1, 0, 1)},
		esDef{0x06, 0x107, desc(descTeletext, 's', 'w', 'e', 0x09, 0x88)},
		esDef{0x86, 0x108, nil},
		esDef{0x06, 0x109, nil},
	)
	if len(pmt) <= tsPacketSize-4 {
		t.Fatalf("test PMT should span packets, len=%d", len(pmt))
	}
	p := newTSParser()
	p.feed(packets(0, patSection(0x1000), 0, false))
	p.feed(packets(0x1000, pmt, 3, true)) // pointer_field + adaptation field

	got := describe(p.codecs())
	want := strings.Join([]string{
		"video:H.264:256:",
		"audio:AAC (ADTS):257:fin",
		"audio:AAC (ADTS):258:swe",
		"audio:AAC (ADTS):259:eng",
		"audio:AAC (ADTS):260:qaa",
		"audio:AC-3:261:eng",
		"subtitle:DVB subtitles:262:fin",
		"subtitle:Teletext:263:swe",
		"data:SCTE-35:264:",
		"data:Private data:265:",
	}, " | ")
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Packets may be fed in arbitrary chunks (SRT messages are usually 7 packets).
func TestChunkedFeedAndRegistrations(t *testing.T) {
	pmt := pmtSection(reg("CUEI"),
		esDef{0x06, 0x200, reg("AV01")},
		esDef{0x06, 0x201, append(reg("Opus"), lang("fin")...)},
		esDef{0x06, 0x202, reg("CUEI")},
		esDef{0x06, 0x203, reg("BSSD")},
	)
	ts := append(packets(0, patSection(0x300), 0, false), packets(0x300, pmt, 0, false)...)
	p := newTSParser()
	for i := 0; i < len(ts); i += tsPacketSize {
		p.feed(ts[i : i+tsPacketSize])
	}
	got := describe(p.codecs())
	want := "video:AV1:512: | audio:Opus:513:fin | audio:SMPTE 302M (PCM):515: | data:SCTE-35:514:"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// A new PMT version replaces the track list (e.g. an audio track is dropped).
func TestPMTUpdateReplacesTracks(t *testing.T) {
	p := newTSParser()
	p.feed(packets(0, patSection(0x100), 0, false))
	p.feed(packets(0x100, pmtSection(nil, esDef{0x1B, 0x101, nil}, esDef{0x0F, 0x102, lang("fin")}, esDef{0x0F, 0x103, lang("swe")}), 0, false))
	p.feed(packets(0x100, pmtSection(nil, esDef{0x1B, 0x101, nil}, esDef{0x0F, 0x102, lang("fin")}), 0, false))
	if got, want := describe(p.codecs()), "video:H.264:257: | audio:AAC (ADTS):258:fin"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Garbage before the first sync byte and corrupted packets must not break parsing.
func TestResyncAndErrors(t *testing.T) {
	ts := append([]byte{1, 2, 3}, packets(0, patSection(0x100), 0, false)...)
	bad := packets(0x100, pmtSection(nil, esDef{0x1B, 0x101, nil}), 0, false)
	bad[1] |= 0x80 // transport_error_indicator
	ts = append(ts, bad...)
	ts = append(ts, packets(0x100, pmtSection(nil, esDef{0x24, 0x101, nil}), 0, false)...)
	p := newTSParser()
	p.feed(ts)
	if got, want := describe(p.codecs()), "video:HEVC (H.265):257:"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCodecSetsEqualIncludesLanguage(t *testing.T) {
	a := []Codec{{Type: TrackAudio, Name: "AAC", PID: 1, Lang: "fin"}}
	b := []Codec{{Type: TrackAudio, Name: "AAC", PID: 1, Lang: "swe"}}
	if codecSetsEqual(a, b) {
		t.Fatal("different languages must not compare equal")
	}
	if !codecSetsEqual(a, append([]Codec(nil), a...)) {
		t.Fatal("identical sets must compare equal")
	}
}
