package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

func TestRepairPSTMessagePreservesUndetectableCalendarAttachment(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	headers := "Microsoft Mail Internet Headers Version 2.0\r\nFrom: sender@example.test\r\nSubject: Calendar repair\r\n"
	body := "The original searchable body for calendar repair."
	entry := pstreader.ExtractMessage(&pstlib.Message{Identifier: 326, Properties: &properties.Message{TransportMessageHeaders: &headers, Body: &body}}, "Inbox")
	require.NotNil(t, entry)
	content := []byte{0x21, 0x4b, 0x5f, 0xdf, 0x14, 0x09, 0xfc, 0x2b}
	corrected, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: "calendar.txt", MIMEType: "text/calendar", Content: content}})
	require.NoError(t, err)
	parsed, err := msgmime.Parse(corrected)
	require.NoError(t, err)
	var attachmentWarning bool
	for _, issue := range parsed.Issues {
		if issue.Name == enmime.ErrorCharsetDeclaration && !issue.BodyPart {
			attachmentWarning = true
		}
	}
	require.True(t, attachmentWarning, "fixture must trigger a calendar attachment charset warning")
	mimeStart := bytes.Index(corrected, []byte("MIME-Version:"))
	require.NotEqual(t, -1, mimeStart)
	old := append([]byte(headers), corrected[mimeStart:]...)
	dir := t.TempDir()
	const sourceMessageID = "pst-synthetic-charset-326"
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceMessageID, "old", old, time.Time{}, slog.Default()))
	ids, err := st.MessageExistsBatch(src.ID, []string{sourceMessageID})
	require.NoError(t, err)
	messageID := ids[sourceMessageID]
	require.NotZero(t, messageID)
	result, err := repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default())
	require.NoError(t, err)
	assert.Equal(t, 1, result.AttachmentsAdded)
	assert.NotEmpty(t, result.Warnings)
	gotBody, err := st.GetMessageBodyText(messageID)
	require.NoError(t, err)
	assert.Contains(t, gotBody, body)
	_, total, err := st.SearchMessages("searchable", 0, 10)
	require.NoError(t, err)
	assert.Positive(t, total)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	got, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(content)), refs[0].ContentHash)
	result, err = repairPSTMessage(t.Context(), st, src.ID, sourceMessageID, messageID, entry, dir, slog.Default())
	require.NoError(t, err)
	assert.Zero(t, result.AttachmentsAdded)

	// A charset warning must not hide a separately malformed attachment.
	encoded := []byte(base64.StdEncoding.EncodeToString(content))
	require.Contains(t, string(corrected), string(encoded))
	broken := bytes.Replace(corrected, encoded, append(bytes.Clone(encoded), '%'), 1)
	brokenOld := append([]byte(headers), broken[mimeStart:]...)
	const brokenSourceID = "pst-synthetic-charset-broken-326"
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, brokenSourceID, "old", brokenOld, time.Time{}, slog.Default()))
	ids, err = st.MessageExistsBatch(src.ID, []string{brokenSourceID})
	require.NoError(t, err)
	brokenID := ids[brokenSourceID]
	require.NotZero(t, brokenID)
	_, err = repairPSTMessage(t.Context(), st, src.ID, brokenSourceID, brokenID, entry, dir, slog.Default())
	require.ErrorContains(t, err, "Malformed Base64")
	unchanged, err := st.GetMessageRaw(brokenID)
	require.NoError(t, err)
	assert.Equal(t, brokenOld, unchanged)
	refs, err = st.MessageMIMEAttachmentsContext(t.Context(), brokenID)
	require.NoError(t, err)
	assert.Empty(t, refs)
}
