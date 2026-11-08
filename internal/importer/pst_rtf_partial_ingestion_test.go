package importer

import (
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

// Text omitted from the searchable body must produce a warning while the
// useful text and original RTF remain available through the archive.
func TestPSTPartialRTFRecoverySurvivesIngestion(t *testing.T) {
	for _, tc := range []struct {
		name, body, omitted string
	}{
		{"footnote", `{\rtf1\ansi searchablemain {\footnote unindexedfootnote}}`, "unindexedfootnote"},
		{"shape text", `{\rtf1\ansi searchablemain {\shp{\shptxt unindexedshape}}}`, "unindexedshape"},
		{"header", `{\rtf1\ansi searchablemain {\header unindexedheader}}`, "unindexedheader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openIntegrationStore(t)
			src, err := st.GetOrCreateSource("pst", "owner@example.test")
			require.NoError(t, err)
			rtf := pstTestMELA([]byte(tc.body))
			entry := pstreader.ExtractMessage(&pstlib.Message{
				Identifier: 123,
				Properties: &properties.Message{},
				PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
					{ID: 0x1009, Type: pstlib.PropertyTypeBinary, Data: rtf},
				}},
			}, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.BodyError)
			require.Error(t, entry.RTFWarning, "omitted visible content must be reported")
			assert.Contains(t, entry.BodyText, "searchablemain")
			assert.NotContains(t, entry.BodyText, tc.omitted)
			require.NotNil(t, entry.RawRTFAttachment)
			assert.Equal(t, rtf, entry.RawRTFAttachment.Content)

			raw, err := pstreader.BuildRFC5322(entry, nil)
			require.NoError(t, err)
			dir := t.TempDir()
			require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil,
				"pst-partial-rtf", "hash", raw, time.Time{}, slog.Default()))
			results, total, err := st.SearchMessages("searchablemain", 0, 10)
			require.NoError(t, err)
			require.Equal(t, int64(1), total)
			require.Len(t, results, 1)
			body, err := st.GetMessageBodyText(results[0].ID)
			require.NoError(t, err)
			assert.Contains(t, body, "searchablemain")
			assert.NotContains(t, body, tc.omitted)
			_, total, err = st.SearchMessages(tc.omitted, 0, 10)
			require.NoError(t, err)
			assert.Zero(t, total)
			stored, err := st.GetMessageRaw(results[0].ID)
			require.NoError(t, err)
			parsed, err := msgmime.Parse(stored)
			require.NoError(t, err)
			require.Len(t, parsed.Attachments, 1)
			assert.Equal(t, "original-pst-rtf-compressed.bin", parsed.Attachments[0].Filename)
			assert.Equal(t, rtf, parsed.Attachments[0].Content)
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), results[0].ID)
			require.NoError(t, err)
			require.Len(t, refs, 1)
			storedRTF, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
			require.NoError(t, err)
			assert.Equal(t, rtf, storedRTF)
		})
	}
}
