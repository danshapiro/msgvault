package importer

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	msgmime "go.kenn.io/msgvault/internal/mime"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

// A malformed leading line must not make MIME readers treat the whole message,
// including the encoded attachments, as plain text.
func TestPSTMalformedTransportHeadersSurviveIngestion(t *testing.T) {
	for _, tc := range []struct {
		name, leading, trailing string
	}{
		{name: "colonless leading line", leading: "Malformed original header line\r\n"},
		{name: "orphan folded line", leading: "\tOrphan original continuation\r\n"},
		{name: "invalid field name", leading: "Invalid header name: source data\r\n"},
		{name: "embedded separator and trailing data", trailing: "\r\nOriginal trailing data\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openIntegrationStore(t)
			src, err := st.GetOrCreateSource("pst", "owner@example.test")
			require.NoError(t, err)
			headers := tc.leading + "From: Original Sender <sender@example.test>\r\n" +
				"To: recipient@example.test\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\n" +
				"Subject: Malformed header recovery\r\n" + tc.trailing
			rtf := pstTestMELA([]byte(`{\rtf1\ansi Original RTF text searchablertfmessage}`))
			entry := pstreader.ExtractMessage(&pstlib.Message{
				Identifier: 123, Properties: &properties.Message{TransportMessageHeaders: &headers},
				PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
					{ID: 0x1009, Type: pstlib.PropertyTypeBinary, Data: rtf},
				}},
			}, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.HeaderError)
			require.NoError(t, entry.BodyError)
			require.NotNil(t, entry.RawRTFAttachment)
			assert.Contains(t, entry.BodyText, "searchablertfmessage")
			original := []byte{0, 0xff, 1, 2, 3, '\n'}
			raw, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: "ordinary.bin", MIMEType: "application/octet-stream", Content: original}})
			require.NoError(t, err)

			// net/mail rejects malformed field names and misplaced continuations,
			// while the MIME parser exercises multipart body/attachment decoding.
			message, err := mail.ReadMessage(bytes.NewReader(raw))
			require.NoError(t, err)
			mediaType, _, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
			require.NoError(t, err)
			assert.Equal(t, "multipart/mixed", mediaType)
			encoded := strings.Join(strings.Fields(message.Header.Get("X-Msgvault-Original-Transport-Headers")), "")
			preserved, err := base64.RawURLEncoding.DecodeString(encoded)
			require.NoError(t, err)
			assert.Equal(t, []byte(headers), preserved)
			parsed, err := msgmime.Parse(raw)
			require.NoError(t, err)
			require.Len(t, parsed.From, 1)
			assert.Equal(t, "sender@example.test", parsed.From[0].Email)
			require.Len(t, parsed.To, 1)
			assert.Equal(t, "recipient@example.test", parsed.To[0].Email)
			assert.Contains(t, parsed.BodyText, "searchablertfmessage")
			want := map[string][]byte{"original-pst-rtf-compressed.bin": rtf, "ordinary.bin": original}
			require.Len(t, parsed.Attachments, len(want))
			for _, attachment := range parsed.Attachments {
				expected, ok := want[attachment.Filename]
				require.True(t, ok)
				assert.Equal(t, expected, attachment.Content)
			}

			dir := t.TempDir()
			for range 2 {
				require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-malformed-header", "hash", raw, time.Time{}, slog.Default()))
			}
			var id, count int64
			require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-malformed-header").Scan(&id))
			require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-malformed-header").Scan(&count))
			assert.Equal(t, int64(1), count)
			stored, err := st.GetMessageRaw(id)
			require.NoError(t, err)
			assert.Equal(t, raw, stored)
			results, total, err := st.SearchMessages("searchablertfmessage", 0, 10)
			require.NoError(t, err)
			require.Equal(t, int64(1), total)
			require.Len(t, results, 1)
			assert.Equal(t, id, results[0].ID)
			assert.Equal(t, "sender@example.test", results[0].FromEmail)
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
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
