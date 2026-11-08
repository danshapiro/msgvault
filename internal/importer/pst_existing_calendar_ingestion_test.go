package importer

import (
	"encoding/binary"
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

// An unusable source calendar must remain in the archived MIME while a usable
// fallback is generated from the original meeting properties.
func TestPSTExistingCalendarFallbackSurvivesIngestion(t *testing.T) {
	validCalendar := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Test//EN\r\nBEGIN:VEVENT\r\nUID:source@example.test\r\nDTSTAMP:20240304T143000Z\r\nDTSTART:20240304T153000Z\r\nDTEND:20240304T163000Z\r\nSUMMARY:Original event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	for _, tc := range []struct {
		name         string
		sourceName   string
		source       []byte
		wantFallback string
		missingEnd   bool
		wantWarning  bool
	}{
		{name: "empty calendar", sourceName: "source.ics", source: []byte{}, wantFallback: "meeting.ics"},
		{name: "malformed calendar", sourceName: "source.ics", source: []byte("source"), wantFallback: "meeting.ics"},
		{name: "incomplete event", sourceName: "source.ics", source: []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n"), wantFallback: "meeting.ics"},
		{name: "malformed source uses generated filename", sourceName: "meeting.ics", source: []byte("bad original"), wantFallback: "meeting-2.ics"},
		{name: "invalid source and no event end", sourceName: "source.ics", source: []byte("bad original"), missingEnd: true, wantWarning: true},
		{name: "existing valid event", sourceName: "source.ics", source: validCalendar},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openIntegrationStore(t)
			src, err := st.GetOrCreateSource("pst", "owner@example.test")
			require.NoError(t, err)
			stamp := func(v time.Time) []byte {
				data := make([]byte, 8)
				binary.LittleEndian.PutUint64(data, uint64(v.Unix()+11644473600)*10000000)
				return data
			}
			start := time.Date(2024, time.March, 4, 15, 30, 0, 0, time.UTC)
			msg := &pstlib.Message{Identifier: 123, Properties: &properties.Appointment{}, File: &pstlib.File{NameToIDMap: &pstlib.NameToIDMap{NameToID: map[int]int{
				int(pstlib.PropertySetAppointment)<<32 | 0x820d: 0x8001,
				int(pstlib.PropertySetAppointment)<<32 | 0x820e: 0x8002,
			}}}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
				{ID: 0x001a, Type: pstlib.PropertyTypeString8, Data: []byte("IPM.Schedule.Meeting.Request\x00")},
				{ID: 0x0037, Type: pstlib.PropertyTypeString8, Data: []byte("Actual meeting\x00")},
				{ID: 0x1000, Type: pstlib.PropertyTypeString8, Data: []byte("Original message text\x00")},
				{ID: 0x0039, Type: pstlib.PropertyTypeTime, Data: stamp(start.Add(-time.Hour))},
				{ID: 0x8001, Type: pstlib.PropertyTypeTime, Data: stamp(start)},
				{ID: 0x8002, Type: pstlib.PropertyTypeTime, Data: stamp(start.Add(time.Hour))},
			}}}
			if tc.missingEnd {
				msg.PropertyContext.Properties = msg.PropertyContext.Properties[:len(msg.PropertyContext.Properties)-1]
			}
			entry := pstreader.ExtractMessage(msg, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.HeaderError)
			require.NoError(t, entry.BodyError)
			require.NoError(t, entry.DateError)
			entry.PSTArchiveID = "archive-example"
			source := pstreader.AttachmentEntry{Filename: tc.sourceName, MIMEType: "text/calendar", Content: tc.source}
			if tc.wantWarning {
				assert.NotEmpty(t, entry.MeetingICSOmission([]pstreader.AttachmentEntry{source}))
			} else {
				assert.Empty(t, entry.MeetingICSOmission([]pstreader.AttachmentEntry{source}))
			}
			raw, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{source})
			require.NoError(t, err)
			dir := t.TempDir()
			require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-calendar-123", "hash", raw, time.Time{}, slog.Default()))
			var id int64
			require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-calendar-123").Scan(&id))
			stored, err := st.GetMessageRaw(id)
			require.NoError(t, err)
			parsed, err := msgmime.Parse(stored)
			require.NoError(t, err)
			assert.Contains(t, parsed.BodyText, "Original message text")
			parts := map[string]msgmime.Attachment{}
			for _, part := range parsed.Attachments {
				parts[part.Filename] = part
			}
			require.Contains(t, parts, tc.sourceName)
			assert.Equal(t, tc.source, parts[tc.sourceName].Content)
			if tc.wantFallback != "" {
				require.Contains(t, parts, tc.wantFallback)
				assert.Equal(t, "text/calendar", parts[tc.wantFallback].ContentType)
				assert.Contains(t, string(parts[tc.wantFallback].Content), "DTSTART:20240304T153000Z\r\n")
				assert.Contains(t, string(parts[tc.wantFallback].Content), "DTEND:20240304T163000Z\r\n")
			} else {
				assert.NotContains(t, parts, "meeting.ics")
			}
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(t, err)
			storedNames := make(map[string]bool, len(refs))
			for _, ref := range refs {
				part, ok := parts[ref.Filename]
				require.True(t, ok)
				data, readErr := os.ReadFile(filepath.Join(dir, ref.StoragePath))
				require.NoError(t, readErr)
				assert.Equal(t, part.Content, data)
				storedNames[ref.Filename] = true
			}
			assert.True(t, storedNames["pst-meeting-properties.json"])
			if len(tc.source) > 0 {
				assert.True(t, storedNames[tc.sourceName])
			}
			if tc.wantFallback != "" {
				assert.True(t, storedNames[tc.wantFallback])
			}
		})
	}
}
