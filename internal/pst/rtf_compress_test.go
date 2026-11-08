package pst

import (
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These fixed LZFu examples are from compressed_rtf's independent test suite:
// https://github.com/delimitry/compressed_rtf/blob/master/tests/tests.py
//
// The MIT License (MIT)
// Copyright (c) 2016 Dmitry Alimov
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
func TestDecodeCompressedRTF_IndependentExamples(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []byte
		want  string
	}{
		{"compressed dictionary references", []byte("-\x00\x00\x00+\x00\x00\x00LZFu\xf1\xc5\xc7\xa7\x03\x00\n\x00rcpg125B2\n\xf3 hel\t\x00 bw\x05\xb0ld}\n\x80\x0f\xa0"), "{\\rtf1\\ansi\\ansicpg1252\\pard hello world}\r\n"},
		{"compressed overlapping references", []byte("\x1a\x00\x00\x00\x1c\x00\x00\x00LZFu\xe2\xd4KQA\x00\x04 WXYZ\rn}\x01\x0e\xb0"), "{\\rtf1 WXYZWXYZWXYZWXYZWXYZ}"},
		{"uncompressed", []byte(".\x00\x00\x00\"\x00\x00\x00MELA\x00\x00\x00\x00{\\rtf1\\ansi\\ansicpg1252\\pard test}"), "{\\rtf1\\ansi\\ansicpg1252\\pard test}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCompressedRTF(tc.input, 1024)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.want), got)
		})
	}
}

func TestDecodeCompressedRTF_RejectsDamagedAndOversized(t *testing.T) {
	valid := []byte("-\x00\x00\x00+\x00\x00\x00LZFu\xf1\xc5\xc7\xa7\x03\x00\n\x00rcpg125B2\n\xf3 hel\t\x00 bw\x05\xb0ld}\n\x80\x0f\xa0")
	mutate := func(f func([]byte)) []byte { out := append([]byte(nil), valid...); f(out); return out }
	compressed := func(rawLen uint32, payload []byte) []byte {
		out := make([]byte, 16+len(payload))
		binary.LittleEndian.PutUint32(out[:4], uint32(len(out)-4))
		binary.LittleEndian.PutUint32(out[4:8], rawLen)
		copy(out[8:12], "LZFu")
		binary.LittleEndian.PutUint32(out[12:16], ^crc32.Update(^uint32(0), crc32.IEEETable, payload))
		copy(out[16:], payload)
		return out
	}
	for _, tc := range []struct {
		name  string
		input []byte
		limit int
	}{
		{"short header", []byte{1, 2, 3}, 1024},
		{"length mismatch", mutate(func(b []byte) { b[0]++ }), 1024},
		{"invalid magic", mutate(func(b []byte) { b[8] = 'X' }), 1024},
		{"invalid checksum", mutate(func(b []byte) { b[12] ^= 1 }), 1024},
		{"truncated copy token", compressed(3, []byte{1, 0}), 1024},
		{"premature terminator", compressed(3, []byte{1, 0x0c, 0xf0}), 1024},
		{"missing terminator", compressed(1, []byte{0, 'x'}), 1024},
		{"uninitialized dictionary", compressed(2, []byte{1, 0xff, 0xf0}), 1024},
		{"copy exceeds declared output", compressed(1, []byte{1, 0, 0x0f}), 1024},
		{"trailing bytes after terminator", compressed(0, []byte{1, 0x0c, 0xf0, 1}), 1024},
		{"oversized declared output", mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[4:8], 1025) }), 1024},
		{"zero maximum", valid, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCompressedRTF(tc.input, tc.limit)
			require.Error(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestDecodeCompressedRTF_WrapsDictionary(t *testing.T) {
	// Literal-only stream crosses the circular buffer boundary twice. The
	// marker points at the final write position and terminates the stream.
	want := make([]byte, 9000)
	for i := range want {
		want[i] = byte('a' + i%26)
	}
	var payload []byte
	for pos := 0; pos < len(want); {
		count := min(8, len(want)-pos)
		payload = append(payload, 0)
		payload = append(payload, want[pos:pos+count]...)
		pos += count
		if count < 8 {
			payload[len(payload)-count-1] = 1 << count
			marker := uint16((len(rtfInitialDictionary) + len(want)) % 4096 << 4)
			payload = binary.BigEndian.AppendUint16(payload, marker)
			break
		}
	}
	if len(want)%8 == 0 {
		payload = append(payload, 1)
		marker := uint16((len(rtfInitialDictionary) + len(want)) % 4096 << 4)
		payload = binary.BigEndian.AppendUint16(payload, marker)
	}
	data := make([]byte, 16, 16+len(payload))
	binary.LittleEndian.PutUint32(data[:4], uint32(12+len(payload)))
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(want)))
	copy(data[8:12], "LZFu")
	binary.LittleEndian.PutUint32(data[12:16], ^crc32.Update(^uint32(0), crc32.IEEETable, payload))
	data = append(data, payload...)
	got, err := decodeCompressedRTF(data, len(want))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func FuzzDecodeCompressedRTF(f *testing.F) {
	f.Add([]byte("-\x00\x00\x00+\x00\x00\x00LZFu\xf1\xc5\xc7\xa7\x03\x00\n\x00rcpg125B2\n\xf3 hel\t\x00 bw\x05\xb0ld}\n\x80\x0f\xa0"))
	f.Add([]byte("\x0c\x00\x00\x00\x00\x00\x00\x00MELA\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := decodeCompressedRTF(data, 256)
		if err == nil {
			require.GreaterOrEqual(t, len(data), 16)
			assert.LessOrEqual(t, len(out), 256)
			assert.Equal(t, uint32(len(out)), binary.LittleEndian.Uint32(data[4:8]))
		}
	})
}
