package relay

// Minimal MPEG-TS parser that discovers the PAT -> PMT -> elementary streams
// to report every video/audio/subtitle/data track with its language. SRT
// carries MPEG-TS, so we sniff the 188-byte packets as they pass through the
// relay. PSI sections are reassembled across packets, so large PMTs (many
// audio/subtitle tracks) are read completely.

import "sort"

const tsPacketSize = 188
const tsSyncByte = 0x47

// Track types reported in Codec.Type.
const (
	TrackVideo    = "video"
	TrackAudio    = "audio"
	TrackSubtitle = "subtitle"
	TrackData     = "data"
)

// streamTypes maps PMT stream_type values to (type, name). stream_type 0x06
// (PES private data) is resolved from descriptors in classifyES instead.
var streamTypes = map[int][2]string{
	0x01: {TrackVideo, "MPEG-1 Video"},
	0x02: {TrackVideo, "MPEG-2 Video"},
	0x10: {TrackVideo, "MPEG-4 Visual"},
	0x1B: {TrackVideo, "H.264"},
	0x24: {TrackVideo, "HEVC (H.265)"},
	0x33: {TrackVideo, "VVC (H.266)"},
	0x03: {TrackAudio, "MPEG-1 Audio"},
	0x04: {TrackAudio, "MPEG-2 Audio"},
	0x0F: {TrackAudio, "AAC (ADTS)"},
	0x11: {TrackAudio, "AAC (LATM)"},
	0x81: {TrackAudio, "AC-3"},
	0x87: {TrackAudio, "E-AC-3"},
	0x90: {TrackAudio, "PCM"},
	0x15: {TrackData, "ID3 metadata"},
	0x86: {TrackData, "SCTE-35"},
}

// Descriptor tags used for classification.
const (
	descRegistration = 0x05
	descISO639       = 0x0A
	descTeletext     = 0x56
	descSubtitling   = 0x59
	descAC3          = 0x6A
	descEAC3         = 0x7A
	descDTS          = 0x7B
	descAACDVB       = 0x7C
)

// registrationNames maps registration descriptor format_identifiers.
var registrationNames = map[string][2]string{
	"AV01": {TrackVideo, "AV1"},
	"Opus": {TrackAudio, "Opus"},
	"AC-3": {TrackAudio, "AC-3"},
	"EAC3": {TrackAudio, "E-AC-3"},
	"BSSD": {TrackAudio, "SMPTE 302M (PCM)"},
	"CUEI": {TrackData, "SCTE-35"},
	"KLVA": {TrackData, "KLV metadata"},
	"ID3 ": {TrackData, "ID3 metadata"},
}

type tsParser struct {
	pmtPIDs  map[int]bool          // PMT PIDs announced by the PAT (all programs)
	programs map[int]map[int]Codec // pmtPID -> elementary PID -> track
	sections map[int]*sectionBuf   // in-progress PSI sections per PID
}

type sectionBuf struct {
	data []byte
}

func newTSParser() *tsParser {
	return &tsParser{
		pmtPIDs:  make(map[int]bool),
		programs: make(map[int]map[int]Codec),
		sections: make(map[int]*sectionBuf),
	}
}

// feed processes a chunk of TS data. It keeps parsing for the whole session:
// PATs/PMTs repeat, and a changed PMT replaces that program's track list.
func (p *tsParser) feed(data []byte) {
	for len(data) >= tsPacketSize {
		if data[0] != tsSyncByte {
			// Try to resync on the next sync byte.
			idx := indexByte(data[1:], tsSyncByte)
			if idx < 0 {
				return
			}
			data = data[idx+1:]
			continue
		}
		p.packet(data[:tsPacketSize])
		data = data[tsPacketSize:]
	}
}

func (p *tsParser) packet(pkt []byte) {
	pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
	if pid != 0 && !p.pmtPIDs[pid] {
		return
	}
	if pkt[1]&0x80 != 0 { // transport_error_indicator
		delete(p.sections, pid)
		return
	}
	pusi := pkt[1]&0x40 != 0
	adaptation := (pkt[3] >> 4) & 0x3
	if adaptation == 0 || adaptation == 2 { // no payload
		return
	}
	pos := 4
	if adaptation == 3 {
		pos += int(pkt[4]) + 1
	}
	if pos >= tsPacketSize {
		return
	}
	payload := pkt[pos:]

	if !pusi {
		// Continuation of a section that started in an earlier packet.
		if sb := p.sections[pid]; sb != nil {
			sb.data = append(sb.data, payload...)
			p.trySection(pid)
		}
		return
	}

	pointer := int(payload[0])
	payload = payload[1:]
	if pointer > len(payload) {
		delete(p.sections, pid)
		return
	}
	// Bytes before the pointer finish the previous section.
	if sb := p.sections[pid]; sb != nil {
		sb.data = append(sb.data, payload[:pointer]...)
		p.trySection(pid)
	}
	// A new section starts here.
	p.sections[pid] = &sectionBuf{data: append([]byte(nil), payload[pointer:]...)}
	p.trySection(pid)
}

// trySection parses the buffered section on pid once it is complete.
func (p *tsParser) trySection(pid int) {
	sb := p.sections[pid]
	if sb == nil || len(sb.data) < 3 {
		return
	}
	if sb.data[0] == 0xFF { // stuffing: no section
		delete(p.sections, pid)
		return
	}
	sectionLen := int(sb.data[1]&0x0F)<<8 | int(sb.data[2])
	if sectionLen > 1021 {
		delete(p.sections, pid)
		return
	}
	total := 3 + sectionLen
	if len(sb.data) < total {
		return // wait for more packets
	}
	sec := sb.data[:total]
	delete(p.sections, pid)

	switch {
	case pid == 0 && sec[0] == 0x00:
		p.parsePAT(sec)
	case sec[0] == 0x02:
		p.parsePMT(pid, sec)
	}
}

func (p *tsParser) parsePAT(sec []byte) {
	end := len(sec) - 4 // minus CRC
	for pos := 8; pos+4 <= end; pos += 4 {
		programNumber := int(sec[pos])<<8 | int(sec[pos+1])
		if programNumber != 0 { // 0 = network PID (NIT)
			p.pmtPIDs[int(sec[pos+2]&0x1F)<<8|int(sec[pos+3])] = true
		}
	}
}

func (p *tsParser) parsePMT(pmtPID int, sec []byte) {
	if len(sec) < 16 {
		return
	}
	end := len(sec) - 4 // minus CRC
	programInfoLen := int(sec[10]&0x0F)<<8 | int(sec[11])
	pos := 12 + programInfoLen
	if pos > end {
		return
	}
	// SCTE-35 is sometimes signalled with a program-level CUEI registration.
	progCUEI := descriptorHasFourCC(sec, 12, pos, "CUEI")

	tracks := make(map[int]Codec)
	for pos+5 <= end {
		streamType := int(sec[pos])
		esPID := int(sec[pos+1]&0x1F)<<8 | int(sec[pos+2])
		esInfoLen := int(sec[pos+3]&0x0F)<<8 | int(sec[pos+4])
		dStart := pos + 5
		dEnd := dStart + esInfoLen
		if dEnd > end {
			dEnd = end
		}
		if c, ok := classifyES(streamType, sec, dStart, dEnd, progCUEI); ok {
			c.PID = esPID
			tracks[esPID] = c
		}
		pos = dEnd
	}
	if len(tracks) > 0 {
		p.programs[pmtPID] = tracks
	}
}

// classifyES works out the track type, codec name and language of one
// elementary stream from its stream_type and descriptors.
func classifyES(streamType int, b []byte, start, end int, progCUEI bool) (Codec, bool) {
	var c Codec
	if st, ok := streamTypes[streamType]; ok {
		c.Type, c.Name = st[0], st[1]
	}
	var reg string
	forEachDescriptor(b, start, end, func(tag byte, d []byte) {
		switch tag {
		case descISO639:
			if len(d) >= 3 && c.Lang == "" {
				c.Lang = langCode(d[:3])
			}
		case descRegistration:
			if len(d) >= 4 {
				reg = string(d[:4])
			}
		case descSubtitling:
			if streamType == 0x06 {
				c.Type, c.Name = TrackSubtitle, "DVB subtitles"
			}
			if len(d) >= 3 && c.Lang == "" {
				c.Lang = langCode(d[:3])
			}
		case descTeletext:
			if streamType == 0x06 {
				c.Type, c.Name = TrackSubtitle, "Teletext"
			}
			if len(d) >= 3 && c.Lang == "" {
				c.Lang = langCode(d[:3])
			}
		case descAC3:
			if streamType == 0x06 {
				c.Type, c.Name = TrackAudio, "AC-3"
			}
		case descEAC3:
			if streamType == 0x06 {
				c.Type, c.Name = TrackAudio, "E-AC-3"
			}
		case descDTS:
			if streamType == 0x06 {
				c.Type, c.Name = TrackAudio, "DTS"
			}
		case descAACDVB:
			if streamType == 0x06 {
				c.Type, c.Name = TrackAudio, "AAC"
			}
		}
	})
	if r, ok := registrationNames[reg]; ok && (streamType == 0x06 || c.Name == "") {
		c.Type, c.Name = r[0], r[1]
	}
	if c.Name == "" && progCUEI && streamType == 0x86 {
		c.Type, c.Name = TrackData, "SCTE-35"
	}
	if c.Name == "" && streamType == 0x06 {
		c.Type, c.Name = TrackData, "Private data"
	}
	return c, c.Name != ""
}

// langCode returns a printable 3-letter ISO 639 code, or "" if it isn't one.
func langCode(b []byte) string {
	for _, x := range b {
		if !(x >= 'a' && x <= 'z' || x >= 'A' && x <= 'Z') {
			return ""
		}
	}
	return string(b)
}

// forEachDescriptor walks the descriptor loop in b[start:end).
func forEachDescriptor(b []byte, start, end int, fn func(tag byte, data []byte)) {
	for start+2 <= end {
		dlen := int(b[start+1])
		dstart := start + 2
		dend := dstart + dlen
		if dend > end {
			return
		}
		fn(b[start], b[dstart:dend])
		start = dend
	}
}

// done reports whether at least one PMT with tracks has been parsed.
func (p *tsParser) done() bool {
	return len(p.programs) > 0
}

var trackOrder = map[string]int{TrackVideo: 0, TrackAudio: 1, TrackSubtitle: 2, TrackData: 3}

// codecs returns every track (one entry per elementary PID), ordered video,
// audio, subtitles, data and by PID within each type.
func (p *tsParser) codecs() []Codec {
	byPID := make(map[int]Codec)
	for _, tracks := range p.programs {
		for pid, c := range tracks {
			byPID[pid] = c
		}
	}
	out := make([]Codec, 0, len(byPID))
	for _, c := range byPID {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := trackOrder[out[i].Type], trackOrder[out[j].Type]
		if oi != oj {
			return oi < oj
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// descriptorHasFourCC reports whether the descriptor loop in b[start:end)
// has a registration descriptor whose format_identifier is fourcc.
func descriptorHasFourCC(b []byte, start, end int, fourcc string) bool {
	found := false
	forEachDescriptor(b, start, end, func(tag byte, d []byte) {
		if tag == descRegistration && len(d) >= 4 && string(d[:4]) == fourcc {
			found = true
		}
	})
	return found
}
