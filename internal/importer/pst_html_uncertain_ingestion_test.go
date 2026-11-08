package importer

import (
	"encoding/binary"
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

// The charset sniffer examines only the beginning of the HTML. Search and the
// archived MIME must instead reflect the entire valid UTF-8 source.
func TestPSTUndeclaredHTMLUTF8AfterSniffWindowSurvivesIngestion(t *testing.T) {
	st := openIntegrationStore(t)
	require.True(t, st.FTS5Available())
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	html := []byte("<html><body>" + strings.Repeat("ordinary prefix ", 90) + "café uniquefindable</body></html>")
	entry := pstreader.ExtractMessage(&pstlib.Message{Identifier: 125, Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: html},
	}}}, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	assert.Contains(t, entry.BodyHTML, "café uniquefindable")
	assert.NotContains(t, entry.BodyHTML, "cafÃ©")
	require.NotNil(t, entry.RawHTMLAttachment)
	assert.Equal(t, html, entry.RawHTMLAttachment.Content)
	raw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-undeclared-utf8", "hash", raw, time.Time{}, slog.Default()))
	results, total, err := st.SearchMessages("uniquefindable", 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, results, 1)
	accented, total, err := st.SearchMessages("café", 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, accented, 1)
	assert.Equal(t, results[0].ID, accented[0].ID)
	body, err := st.GetMessageBodyText(results[0].ID)
	require.NoError(t, err)
	assert.Contains(t, body, "café uniquefindable")
	stored, err := st.GetMessageRaw(results[0].ID)
	require.NoError(t, err)
	parsed, err := msgmime.Parse(stored)
	require.NoError(t, err)
	assert.Contains(t, parsed.BodyHTML, "café uniquefindable")
	require.Len(t, parsed.Attachments, 1)
	assert.Equal(t, "original-pst-html.bin", parsed.Attachments[0].Filename)
	assert.Equal(t, html, parsed.Attachments[0].Content)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), results[0].ID)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	storedBytes, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
	require.NoError(t, err)
	assert.Equal(t, html, storedBytes)
}

// When neither HTML nor plain text can be decoded, both original body
// properties must remain available after normal message ingestion.
func TestPSTUndecodableHTMLAndRTFSurviveIngestion(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	html := []byte("<meta charset=x-unknown><p>raw \xff html</p>\x00")
	rtfText := []byte(`{\rtf1\ansi original text}`)
	rtf := make([]byte, 16+len(rtfText))
	binary.LittleEndian.PutUint32(rtf[0:4], uint32(len(rtf)-4))
	binary.LittleEndian.PutUint32(rtf[4:8], uint32(len(rtfText)))
	copy(rtf[8:12], "MELA") // An uncompressed MS-OXRTFCP payload.
	copy(rtf[16:], rtfText)
	entry := pstreader.ExtractMessage(&pstlib.Message{Identifier: 126, Properties: &properties.Message{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x1013, Type: pstlib.PropertyTypeBinary, Data: html},
		{ID: 0x1009, Type: pstlib.PropertyTypeBinary, Data: rtf},
	}}}, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.BodyError)
	ordinary := []byte("ordinary attachment")
	raw, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{{Filename: "ordinary.txt", MIMEType: "text/plain", Content: ordinary}})
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-html-rtf", "hash", raw, time.Time{}, slog.Default()))
	var messageID int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-html-rtf").Scan(&messageID))
	stored, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	parsed, err := msgmime.Parse(stored)
	require.NoError(t, err)
	assert.Contains(t, parsed.BodyText, "original text")
	assert.Empty(t, parsed.BodyHTML)
	want := map[string][]byte{
		"original-pst-html.bin":           html,
		"original-pst-rtf-compressed.bin": rtf,
		"ordinary.txt":                    ordinary,
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
		storedBytes, readErr := os.ReadFile(filepath.Join(dir, ref.StoragePath))
		require.NoError(t, readErr)
		assert.Equal(t, content, storedBytes)
	}
}
