package importer

import (
	"log/slog"
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

func TestPSTDamagedHTMLSurvivesIngestionAndSearch(t *testing.T) {
	for _, tc := range []struct {
		name         string
		html         []byte
		properties   []pstlib.Property
		plainBody    string
		search       string
		attachment   bool
		noHTML       bool
		htmlContains string
	}{
		{
			name:       "invalid UTF-8 with a declared charset",
			html:       []byte("<meta charset=utf-8><p>findablemalformed before \xff after</p>\x00"),
			search:     "findablemalformed",
			attachment: true,
		},
		{
			name:       "unsupported codepage with a plain text body",
			html:       []byte("<p>unrecognized codepage \xff</p>\x00"),
			properties: []pstlib.Property{{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{0x39, 0x30, 0, 0}}}, // 12345
			plainBody:  "findableplainbody from the original message",
			search:     "findableplainbody",
		},
		{
			name:       "invalid UTF-8 with conflicting charset and a plain text body",
			html:       []byte("<meta charset=windows-1252><p>partly readable \xff HTML</p>\x00"),
			properties: []pstlib.Property{{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe9, 0xfd, 0, 0}}}, // 65001
			plainBody:  "findableoriginalplain content",
			search:     "findableoriginalplain",
			noHTML:     true,
		},
		{
			name: "invalid UTF-8 meta with conflicting Internet and message codepages",
			html: []byte("<meta charset=utf-8><p>partly readable \xff HTML</p>\x00"),
			properties: []pstlib.Property{
				{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xaf, 0x6f, 0, 0}}, // 28591
				{ID: 0x3ffd, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe4, 0x04, 0, 0}}, // 1252
			},
			plainBody: "findableconflictingcodepages content",
			search:    "findableconflictingcodepages",
			noHTML:    true,
		},
		{
			name: "invalid UTF-8 Internet codepage with a different message codepage",
			html: []byte("<p>partly readable \xff HTML</p>\x00"),
			properties: []pstlib.Property{
				{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe9, 0xfd, 0, 0}}, // 65001
				{ID: 0x3ffd, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe4, 0x04, 0, 0}}, // 1252
			},
			plainBody:    "findableinvalidutf8 content",
			search:       "findableinvalidutf8",
			htmlContains: "\ufffd",
		},
		{
			name:   "unknown declared charset without a plain text body",
			html:   []byte("<meta charset=x-unknown-example><p>not-guessed\xff</p>"),
			noHTML: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openIntegrationStore(t)
			require.True(t, st.FTS5Available())
			src, err := st.GetOrCreateSource("pst", "owner@example.test")
			require.NoError(t, err)
			headers := "From: sender@example.test\r\nTo: recipient@example.test\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\nSubject: Synthetic message\r\n"
			props := append([]pstlib.Property{{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: tc.html}}, tc.properties...)
			message := &pstlib.Message{
				Identifier:      123,
				Properties:      &properties.Message{Body: &tc.plainBody, TransportMessageHeaders: &headers},
				PropertyContext: &pstlib.PropertyContext{Properties: props},
			}
			entry := pstreader.ExtractMessage(message, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.HeaderError)
			require.NoError(t, entry.BodyError)
			originalAttachment := []byte("ordinary attachment content")
			var attachments []pstreader.AttachmentEntry
			want := map[string][]byte{"original-pst-html.bin": tc.html}
			if tc.attachment {
				attachments = append(attachments, pstreader.AttachmentEntry{Filename: "ordinary.txt", MIMEType: "text/plain", Content: originalAttachment})
				want["ordinary.txt"] = originalAttachment
			}
			raw, err := pstreader.BuildRFC5322(entry, attachments)
			require.NoError(t, err)
			dir := t.TempDir()
			require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-html-recovery", "hash", raw, time.Time{}, slog.Default()))

			var messageID int64
			require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-html-recovery").Scan(&messageID))
			if tc.search != "" {
				results, total, searchErr := st.SearchMessages(tc.search, 0, 10)
				require.NoError(t, searchErr)
				require.Equal(t, int64(1), total)
				require.Len(t, results, 1)
				assert.Equal(t, messageID, results[0].ID)
				assert.Equal(t, "sender@example.test", results[0].FromEmail)
				assert.Equal(t, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), results[0].SentAt.UTC())
			}
			body, err := st.GetMessageBodyText(messageID)
			require.NoError(t, err)
			if tc.search == "" {
				assert.Empty(t, body)
			} else {
				assert.Contains(t, body, tc.search)
			}

			stored, err := st.GetMessageRaw(messageID)
			require.NoError(t, err)
			parsed, err := msgmime.Parse(stored)
			require.NoError(t, err)
			if tc.plainBody != "" {
				assert.Contains(t, parsed.BodyText, tc.plainBody)
			}
			if tc.noHTML {
				assert.Empty(t, parsed.BodyHTML)
			}
			if tc.htmlContains != "" {
				assert.Contains(t, parsed.BodyHTML, tc.htmlContains)
			}
			require.Len(t, parsed.Attachments, len(want))
			for _, attachment := range parsed.Attachments {
				content, ok := want[attachment.Filename]
				require.True(t, ok, "unexpected attachment %q", attachment.Filename)
				assert.Equal(t, content, attachment.Content)
			}
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
			require.NoError(t, err)
			require.Len(t, refs, len(want))
			for _, ref := range refs {
				content, ok := want[ref.Filename]
				require.True(t, ok, "unexpected stored attachment %q", ref.Filename)
				storedContent, readErr := os.ReadFile(filepath.Join(dir, ref.StoragePath))
				require.NoError(t, readErr)
				assert.Equal(t, content, storedContent)
			}
		})
	}
}

func TestPSTUTF8CodepageOverridesConflictingHTMLCharset(t *testing.T) {
	st := openIntegrationStore(t)
	require.True(t, st.FTS5Available())
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	plain := "originalplaintext searchneedle"
	html := []byte("<html><head><meta charset=windows-1252></head><body>café htmloriginal</body></html>")
	message := &pstlib.Message{Identifier: 124, Properties: &properties.Message{Body: &plain}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: html},
		{ID: 0x3fde, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe9, 0xfd, 0, 0}}, // 65001
		{ID: 0x3ffd, Type: pstlib.PropertyTypeInteger32, Data: []byte{0xe4, 0x04, 0, 0}}, // 1252
	}}}
	entry := pstreader.ExtractMessage(message, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	raw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, t.TempDir(), nil, "pst-utf8-conflict", "hash", raw, time.Time{}, slog.Default()))
	results, total, err := st.SearchMessages("searchneedle", 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, results, 1)
	stored, err := st.GetMessageRaw(results[0].ID)
	require.NoError(t, err)
	parsed, err := msgmime.Parse(stored)
	require.NoError(t, err)
	assert.Contains(t, parsed.BodyText, plain)
	assert.Contains(t, parsed.BodyHTML, "café htmloriginal")
	assert.NotContains(t, parsed.BodyHTML, "cafÃ©")
	assert.NotContains(t, strings.ToLower(parsed.BodyHTML), "windows-1252")
	require.Len(t, parsed.Attachments, 1)
	assert.Equal(t, "original-pst-html.bin", parsed.Attachments[0].Filename)
	assert.Equal(t, html, parsed.Attachments[0].Content)
}
