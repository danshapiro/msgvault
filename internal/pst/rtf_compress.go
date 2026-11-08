package pst

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// The fixed initial dictionary specified by MS-OXRTFCP section 3.1.1.1.
const rtfInitialDictionary = "{\\rtf1\\ansi\\mac\\deff0\\deftab720{\\fonttbl;}{\\f0\\fnil \\froman \\fswiss \\fmodern \\fscript \\fdecor MS Sans SerifSymbolArialTimes New RomanCourier{\\colortbl\\red0\\green0\\blue0\r\n\\par \\pard\\plain\\f0\\fs20\\b\\i\\u\\tab\\tx"

// decodeCompressedRTF decodes the PidTagRtfCompressed representation. It
// rejects inconsistent sizes, bad checksums, malformed references, and output
// over the caller's bound. Callers can retain the original bytes on failure.
func decodeCompressedRTF(data []byte, maxOutput int) ([]byte, error) {
	if len(data) < 16 {
		return nil, errors.New("compressed RTF header is truncated")
	}
	if maxOutput <= 0 {
		return nil, errors.New("compressed RTF output limit must be positive")
	}
	compressedSize := binary.LittleEndian.Uint32(data[:4])
	rawSize := binary.LittleEndian.Uint32(data[4:8])
	if compressedSize < 12 || uint64(compressedSize)+4 != uint64(len(data)) {
		return nil, errors.New("compressed RTF size does not match source bytes")
	}
	if uint64(rawSize) > uint64(maxOutput) {
		return nil, fmt.Errorf("compressed RTF output exceeds %d bytes", maxOutput)
	}
	payload := data[16:]
	checksum := binary.LittleEndian.Uint32(data[12:16])
	switch string(data[8:12]) {
	case "MELA":
		if checksum != 0 || uint64(len(payload)) != uint64(rawSize) {
			return nil, errors.New("uncompressed RTF has invalid checksum or size")
		}
		return append([]byte(nil), payload...), nil
	case "LZFu":
		// MS-OXRTFCP uses the reflected IEEE polynomial with both initial
		// value and final XOR set to zero, unlike the usual IEEE CRC-32.
		if ^crc32.Update(^uint32(0), crc32.IEEETable, payload) != checksum {
			return nil, errors.New("compressed RTF checksum mismatch")
		}
	default:
		return nil, errors.New("unsupported RTF compression signature")
	}

	const dictionarySize = 4096
	var dictionary [dictionarySize]byte
	var initialized [dictionarySize]bool
	copy(dictionary[:], rtfInitialDictionary)
	for i := range len(rtfInitialDictionary) {
		initialized[i] = true
	}
	writePos := len(rtfInitialDictionary)
	out := make([]byte, 0, int(rawSize))
	pos := 0
	for pos < len(payload) {
		flags := payload[pos]
		pos++
		for bit := range 8 {
			if flags&(1<<bit) == 0 {
				if pos >= len(payload) || len(out) >= int(rawSize) {
					return nil, errors.New("compressed RTF literal is truncated or exceeds declared size")
				}
				value := payload[pos]
				pos++
				out = append(out, value)
				dictionary[writePos], initialized[writePos] = value, true
				writePos = (writePos + 1) % dictionarySize
				continue
			}
			if len(payload)-pos < 2 {
				return nil, errors.New("compressed RTF reference is truncated")
			}
			token := binary.BigEndian.Uint16(payload[pos : pos+2])
			pos += 2
			readPos, count := int(token>>4), int(token&0xf)+2
			if readPos == writePos {
				if len(out) != int(rawSize) || pos != len(payload) {
					return nil, errors.New("compressed RTF terminator does not match declared size")
				}
				return out, nil
			}
			if count > int(rawSize)-len(out) {
				return nil, errors.New("compressed RTF reference exceeds declared size")
			}
			for range count {
				if !initialized[readPos] {
					return nil, errors.New("compressed RTF references unavailable dictionary data")
				}
				value := dictionary[readPos]
				out = append(out, value)
				dictionary[writePos], initialized[writePos] = value, true
				writePos = (writePos + 1) % dictionarySize
				readPos = (readPos + 1) % dictionarySize
			}
		}
	}
	return nil, errors.New("compressed RTF terminator is missing")
}
