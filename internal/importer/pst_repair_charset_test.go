package importer

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhillyerd/enmime/v2"
	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	msgmime "go.kenn.io/msgvault/internal/mime"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

const repairCharsetBody = "Sehr geehrte Damen und Herren, ich wende mich wegen der Heizkörper an Sie.\n\nDies ist ein Hinweis zur Ablesung. Bitte senden Sie die Angaben im Laufe der kommenden Woche an die Verwaltung.\n\nMit freundlichen Grüßen"

func TestRepairPSTMessageCharsetWarnings(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		legacy, malformed, textAttachment bool
	}{
		{name: "valid UTF8"},
		{name: "uncertain legacy bytes", legacy: true},
		{name: "malformed attachment", malformed: true},
		{name: "text attachment mismatch", textAttachment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openIntegrationStore(t)
			src, err := st.GetOrCreateSource("pst", "owner@example.test")
			require.NoError(t, err)
			headers := "Microsoft Mail Internet Headers Version 2.0\r\nFrom: sender@example.test\r\nSubject: Charset repair\r\n"
			body := repairCharsetBody
			entry := pstreader.ExtractMessage(&pstlib.Message{Identifier: 325, Properties: &properties.Message{TransportMessageHeaders: &headers, Body: &body}}, "Inbox")
			require.NotNil(t, entry)
			attachment := []byte("binary attachment uniquely retained")
			filename, mimeType := "retained.bin", "application/octet-stream"
			if tc.textAttachment {
				filename, mimeType, attachment = "retained.txt", "text/plain", []byte(repairCharsetBody)
			}
			corrected, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: filename, MIMEType: mimeType, Content: attachment}})
			require.NoError(t, err)
			if tc.textAttachment {
				original := []byte("Content-Type: text/plain; name=retained.txt")
				require.Contains(t, string(corrected), string(original))
				corrected = bytes.Replace(corrected, original, []byte("Content-Type: text/plain; charset=utf-8; name=retained.txt"), 1)
			}
			if tc.legacy {
				for _, pair := range [][2]string{{"=C3=B6", "=F6"}, {"=C3=BC", "=FC"}, {"=C3=9F", "=DF"}} {
					corrected = bytes.ReplaceAll(corrected, []byte(pair[0]), []byte(pair[1]))
				}
			}
			if tc.malformed {
				encoded := []byte(base64.StdEncoding.EncodeToString(attachment))
				require.Contains(t, string(corrected), string(encoded))
				corrected = bytes.Replace(corrected, encoded, []byte("not-valid-base64%%%"), 1)
			}
			// Confirm the fixture actually triggers the parser diagnostic addressed here.
			parsed, err := msgmime.Parse(corrected)
			require.NoError(t, err)
			mismatch := false
			attachmentMismatch := false
			for _, issue := range parsed.Issues {
				if issue.Name == enmime.ErrorCharsetDeclaration {
					mismatch = true
					attachmentMismatch = attachmentMismatch || !issue.BodyPart
				}
			}
			require.True(t, mismatch, "fixture must exercise a charset mismatch")
			if tc.textAttachment {
				require.True(t, attachmentMismatch, "fixture must exercise an attachment charset mismatch")
			}
			mimeStart := bytes.Index(corrected, []byte("MIME-Version:"))
			require.NotEqual(t, -1, mimeStart)
			old := append([]byte(headers), corrected[mimeStart:]...)
			dir := t.TempDir()
			const sourceMessageID = "pst-synthetic-charset-325"
			require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMessageID, "old", old, time.Time{}, slog.Default()))
			ids, err := st.MessageExistsBatch(src.ID, []string{sourceMessageID})
			require.NoError(t, err)
			messageID := ids[sourceMessageID]
			require.NotZero(t, messageID)
			beforeBody, err := st.GetMessageBodyText(messageID)
			require.NoError(t, err)
			var logs bytes.Buffer
			result, err := repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.New(slog.NewTextHandler(&logs, nil)))
			if tc.malformed || tc.textAttachment {
				require.Error(t, err)
				if tc.malformed {
					assert.Contains(t, err.Error(), "Malformed Base64")
				}
				unchanged, readErr := st.GetMessageRaw(messageID)
				require.NoError(t, readErr)
				assert.Equal(t, old, unchanged)
				refs, readErr := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
				require.NoError(t, readErr)
				assert.Empty(t, refs)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, logs.String(), "charset detection warning")
			require.NotEmpty(t, result.Warnings)
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
			require.NoError(t, err)
			require.Len(t, refs, 1)
			got, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
			require.NoError(t, err)
			assert.Equal(t, attachment, got)
			newRaw, err := st.GetMessageRaw(messageID)
			require.NoError(t, err)
			_, oldBody, ok := bytes.Cut(old, []byte("\r\n\r\n"))
			require.True(t, ok)
			_, newBody, ok := bytes.Cut(newRaw, []byte("\r\n\r\n"))
			require.True(t, ok)
			assert.Equal(t, oldBody, newBody)
			gotBody, err := st.GetMessageBodyText(messageID)
			require.NoError(t, err)
			if tc.legacy {
				assert.True(t, result.BodyPreserved)
				assert.Contains(t, strings.Join(result.Warnings, "; "), "body encoding is uncertain")
				assert.Equal(t, beforeBody, gotBody)
			} else {
				assert.Equal(t, strings.TrimSpace(body), strings.TrimSpace(strings.ReplaceAll(gotBody, "\r\n", "\n")))
				_, total, searchErr := st.SearchMessages("Heizkörper", 0, 10)
				require.NoError(t, searchErr)
				assert.Positive(t, total)
			}
		})
	}
}
