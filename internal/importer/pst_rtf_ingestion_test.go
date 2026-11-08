package importer

import (
	"encoding/binary"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	msgmime "go.kenn.io/msgvault/internal/mime"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

// MELA is the uncompressed representation of the PST compressed-RTF
// property. The original property still needs to survive as an attachment.
func pstTestMELA(body []byte) []byte {
	data := make([]byte, 16+len(body))
	binary.LittleEndian.PutUint32(data[0:4], uint32(12+len(body)))
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(body)))
	copy(data[8:12], "MELA")
	copy(data[16:], body)
	return data
}

// Encode a synthetic LZFu stream with literal tokens and an end marker.
// This deliberately does not share the production decoder's implementation.
func pstTestLZFu(body []byte) []byte {
	var payload []byte
	remaining := body
	for len(remaining) >= 8 {
		payload = append(payload, 0)
		payload = append(payload, remaining[:8]...)
		remaining = remaining[8:]
	}
	payload = append(payload, 1<<uint(len(remaining)))
	payload = append(payload, remaining...)
	end := (207 + len(body)) % 4096
	payload = append(payload, byte(end>>4), byte((end&15)<<4))
	data := make([]byte, 16+len(payload))
	binary.LittleEndian.PutUint32(data[0:4], uint32(12+len(payload)))
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(body)))
	copy(data[8:12], "LZFu")
	binary.LittleEndian.PutUint32(data[12:16], ^crc32.Update(^uint32(0), crc32.IEEETable, payload))
	copy(data[16:], payload)
	return data
}

// Exercise source property extraction, generated MIME, durable attachment
// storage and search. These source properties do not contain a text or HTML
// fallback, so a search hit must come from RTF recovery.
func TestPSTRTFOnlyMessageSurvivesIngestion(t *testing.T) {
	for _, tc := range []struct {
		name, search string
		rtf          []byte
		wantWarning  bool
	}{
		{name: "readable RTF", rtf: pstTestMELA([]byte(`{\rtf1\ansi Hello \b meeting\b0\par uniquertfcontent}`)), search: "uniquertfcontent"},
		{name: "compressed RTF", rtf: pstTestLZFu([]byte(`{\rtf1\ansi compressed searchablelzfu}`)), search: "searchablelzfu"},
		{name: "truncated compressed property", rtf: []byte{1, 2, 3, 4}, wantWarning: true},
		{name: "malformed RTF text", rtf: pstTestMELA([]byte(`{\rtf1\ansi unclosed`)), wantWarning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openIntegrationStore(t)
			src, err := st.GetOrCreateSource("pst", "owner@example.test")
			require.NoError(t, err)
			headers := "From: sender@example.test\r\nTo: recipient@example.test\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\nSubject: RTF recovery\r\n"
			entry := pstreader.ExtractMessage(&pstlib.Message{
				Identifier: 123,
				Properties: &properties.Message{TransportMessageHeaders: &headers},
				PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
					{ID: 0x1009, Type: pstlib.PropertyTypeBinary, Data: tc.rtf},
				}},
			}, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.HeaderError)
			require.NoError(t, entry.BodyError)
			require.NoError(t, entry.DateError)
			require.NotNil(t, entry.RawRTFAttachment)
			assert.Equal(t, tc.rtf, entry.RawRTFAttachment.Content)
			if tc.wantWarning {
				require.Error(t, entry.RTFWarning)
				assert.Empty(t, entry.BodyText)
			} else {
				require.NoError(t, entry.RTFWarning)
				assert.Contains(t, entry.BodyText, tc.search)
			}

			ordinary := []byte("ordinary attachment")
			raw, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: "ordinary.txt", MIMEType: "text/plain", Content: ordinary}})
			require.NoError(t, err)
			dir := t.TempDir()
			require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-rtf-only", "hash", raw, time.Time{}, slog.Default()))
			var messageID int64
			require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-rtf-only").Scan(&messageID))
			body, err := st.GetMessageBodyText(messageID)
			require.NoError(t, err)
			if tc.search == "" {
				assert.Empty(t, body)
			} else {
				assert.Contains(t, body, tc.search)
				results, total, err := st.SearchMessages(tc.search, 0, 10)
				require.NoError(t, err)
				require.Equal(t, int64(1), total)
				require.Len(t, results, 1)
				assert.Equal(t, messageID, results[0].ID)
				assert.Equal(t, "sender@example.test", results[0].FromEmail)
			}
			stored, err := st.GetMessageRaw(messageID)
			require.NoError(t, err)
			parsed, err := msgmime.Parse(stored)
			require.NoError(t, err)
			want := map[string][]byte{"original-pst-rtf-compressed.bin": tc.rtf, "ordinary.txt": ordinary}
			require.Len(t, parsed.Attachments, len(want))
			for _, att := range parsed.Attachments {
				expected, ok := want[att.Filename]
				require.True(t, ok)
				assert.Equal(t, expected, att.Content)
			}
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
			require.NoError(t, err)
			require.Len(t, refs, len(want))
			for _, ref := range refs {
				expected, ok := want[ref.Filename]
				require.True(t, ok)
				content, err := os.ReadFile(filepath.Join(dir, ref.StoragePath))
				require.NoError(t, err)
				assert.Equal(t, expected, content)
			}
		})
	}
}
