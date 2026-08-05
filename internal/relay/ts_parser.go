package relay

// Minimal MPEG-TS parser that discovers the PAT -> PMT -> ES stream types
// to report video/audio codecs. SRT carries MPEG-TS, so we sniff the 188-byte
// packets as they pass through the relay.

const tsPacketSize = 188
const tsSyncByte = 0x47

var codecNames = map[int]string{
	0x01: "MPEG-1 Video",
	0x02: "MPEG-2 Video",
	0x03: "MPEG-1 Audio",
	0x04: "MPEG-2 Audio",
	0x0F: "AAC (ADTS)",
	0x11: "AAC (LATM)",
	0x1B: "H.264",
	0x24: "HEVC (H.265)",
	0x06: "AC-3 / Private",
	0x81: "AC-3",
	0x87: "E-AC-3",
	0x90: "PCM",
}

func codecTypeFor(streamType int) string {
	switch streamType {
	case 0x01, 0x02, 0x1B, 0x24:
		return "video"
	case 0x03, 0x04, 0x0F, 0x11, 0x06, 0x81, 0x87, 0x90:
		return "audio"
	default:
		return "other"
	}
}

type tsParser struct {
	pmtPID   int
	pmtFound bool
	found    map[int]Codec
}

func newTSParser() *tsParser {
	return &tsParser{pmtPID: -1, found: make(map[int]Codec)}
}

// feed processes a chunk of TS data, populating p.found when PMT is seen.
func (p *tsParser) feed(data []byte) {
	for len(data) >= tsPacketSize && !p.done() {
		if data[0] != tsSyncByte {
			// Try to resync on the next sync byte.
			idx := indexByte(data, tsSyncByte)
			if idx < 0 {
				return
			}
			data = data[idx:]
			continue
		}

		pkt := data[:tsPacketSize]
		data = data[tsPacketSize:]

		// transport_error_indicator + payload_unit_start_indicator + PID
		pusi := (pkt[1] >> 6) & 0x01
		pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
		adaptation := (pkt[3] >> 4) & 0x3

		// Find where the payload begins.
		payloadStart := 4
		if adaptation == 2 || adaptation == 3 {
			payloadStart += int(pkt[4]) + 1 // adaptation_field_length + the length byte
		}

		// When PUSI is set, the first payload byte is the pointer_field.
		sectionStart := payloadStart
		if pusi == 1 {
			sectionStart++
		}

		if pid == 0 && pusi == 1 {
			// PAT: table_id(0x00), section_length, tsid(2), ver(1), secnum(1), lastnum(1)
			if sectionStart+8 > tsPacketSize {
				continue
			}
			sectionLen := int(pkt[sectionStart+1]&0x0F)<<8 | int(pkt[sectionStart+2])
			end := sectionStart + 3 + sectionLen - 4 // minus CRC
			if end > tsPacketSize {
				end = tsPacketSize
			}
			pos := sectionStart + 8 // first program entry
			for pos+4 <= end {
				programNumber := int(pkt[pos])<<8 | int(pkt[pos+1])
				if programNumber != 0 {
					p.pmtPID = int(pkt[pos+2]&0x1F)<<8 | int(pkt[pos+3])
				}
				pos += 4
			}
		} else if pid == p.pmtPID && pusi == 1 && p.pmtPID >= 0 {
			// PMT: table_id(0x02), section_length, program_number(2), ver(1),
			// secnum(1), lastnum(1), PCR_PID(2), program_info_length(2)
			if sectionStart+12 > tsPacketSize {
				continue
			}
			sectionLen := int(pkt[sectionStart+1]&0x0F)<<8 | int(pkt[sectionStart+2])
			programInfoLen := int(pkt[sectionStart+10]&0x0F)<<8 | int(pkt[sectionStart+11])
			pos := sectionStart + 12 + programInfoLen
			end := sectionStart + 3 + sectionLen - 4 // minus CRC
			if end > tsPacketSize {
				end = tsPacketSize
			}
			for pos+5 <= end {
				streamType := int(pkt[pos])
				elementaryPID := int(pkt[pos+1]&0x1F)<<8 | int(pkt[pos+2])
				if name, ok := codecNames[streamType]; ok {
					c := Codec{Type: codecTypeFor(streamType), Name: name, PID: elementaryPID}
					p.found[elementaryPID] = c
				}
				esInfoLen := int(pkt[pos+3]&0x0F)<<8 | int(pkt[pos+4])
				pos += 5 + esInfoLen
			}
			if len(p.found) > 0 {
				p.pmtFound = true
			}
		}
	}
}

func (p *tsParser) done() bool {
	return p.pmtFound
}

func (p *tsParser) codecs() []Codec {
	out := make([]Codec, 0, len(p.found))
	seen := make(map[string]bool)
	for _, c := range p.found {
		key := c.Type + ":" + c.Name
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
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
