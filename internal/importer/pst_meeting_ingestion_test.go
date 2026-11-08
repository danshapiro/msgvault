package importer

import (
	"database/sql"
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

// A meeting request is decoded as an Appointment by go-pst even when its
// underlying MAPI properties describe a message. Exercise its real extraction,
// MIME construction, ingest, replay and reply resolution with synthetic data.
func TestPSTMeetingRequestIngestionAndReplay(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	date := time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)
	filetime := make([]byte, 8)
	binary.LittleEndian.PutUint64(filetime, uint64(date.Unix()+11644473600)*10000000)
	string8 := func(id uint16, value string) pstlib.Property {
		return pstlib.Property{ID: id, Type: pstlib.PropertyTypeString8, Data: append([]byte(value), 0)}
	}
	calendar := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:meeting@example.test\r\nDTSTART:20240104T150000Z\r\nDTEND:20240104T160000Z\r\nSUMMARY:Synthetic meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	evidence := []byte("synthetic meeting attachment")
	parentID := "<parent@example.test>"
	replyID := "<request@example.test>"
	headers := "From: Organizer <organizer@example.test>\r\nTo: Attendee <attendee@example.test>\r\nCc: observer@example.test\r\nDate: Tue, 02 Jan 2024 15:04:05 +0000\r\nSubject: Synthetic meeting request\r\n"
	msg := &pstlib.Message{Identifier: 123, Properties: &properties.Appointment{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		string8(0x001a, "IPM.Schedule.Meeting.Request"),
		string8(0x007d, headers),
		string8(0x1000, "Meeting request body uniquemeetingtext"),
		string8(0x1035, replyID),
		string8(0x1042, parentID),
		{ID: 0x0039, Type: pstlib.PropertyTypeTime, Data: filetime},
	}}}
	entry := pstreader.ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	require.NoError(t, entry.BodyError)
	require.NoError(t, entry.DateError)
	assert.Equal(t, date, entry.SentAt)
	attachments := []pstreader.AttachmentEntry{
		{Filename: "invite.ics", MIMEType: "text/calendar", Content: calendar},
		{Filename: "agenda.txt", MIMEType: "text/plain", Content: evidence},
	}
	raw, err := pstreader.BuildRFC5322(entry, attachments)
	require.NoError(t, err)
	parsed, err := msgmime.Parse(raw)
	require.NoError(t, err)
	require.Len(t, parsed.From, 1)
	assert.Equal(t, "organizer@example.test", parsed.From[0].Email)
	require.Len(t, parsed.To, 1)
	assert.Equal(t, "attendee@example.test", parsed.To[0].Email)
	require.Len(t, parsed.Cc, 1)
	assert.Equal(t, "observer@example.test", parsed.Cc[0].Email)
	assert.Contains(t, parsed.BodyText, "uniquemeetingtext")
	assert.Equal(t, date, parsed.Date)
	assert.True(t, parsed.HasCalendarPart)
	require.Len(t, parsed.Attachments, 2)
	assert.Equal(t, calendar, parsed.Attachments[0].Content)
	assert.Equal(t, evidence, parsed.Attachments[1].Content)

	ingest := pstMessageIngester(nil)
	sourceID := "pst-synthetic-123"
	for range 2 { // The same source identity cannot create a second message.
		require.NoError(t, ingest(t.Context(), st, src.ID, src.Identifier, dir, nil, sourceID, "request-hash", raw, entry.FallbackDate(time.Now()), slog.Default()))
	}
	var messageID, count int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, sourceID).Scan(&messageID))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, sourceID).Scan(&count))
	assert.Equal(t, int64(1), count)
	stored, err := st.GetMessageRaw(messageID)
	require.NoError(t, err)
	assert.Equal(t, raw, stored)
	body, err := st.GetMessageBodyText(messageID)
	require.NoError(t, err)
	assert.Contains(t, body, "uniquemeetingtext")
	results, total, err := st.SearchMessages("uniquemeetingtext", 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, results, 1)
	assert.Equal(t, messageID, results[0].ID)
	assert.Equal(t, "organizer@example.test", results[0].FromEmail)
	assert.Equal(t, date, results[0].SentAt.UTC())
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	expected := map[string][]byte{"invite.ics": calendar, "agenda.txt": evidence}
	for _, ref := range refs {
		want, ok := expected[ref.Filename]
		require.True(t, ok)
		data, err := os.ReadFile(filepath.Join(dir, ref.StoragePath))
		require.NoError(t, err)
		assert.Equal(t, want, data)
	}

	// Insert the referenced parent after the meeting request, then finish the
	// import as the real PST path does and verify reply resolution.
	parentRaw := []byte("From: parent@example.test\r\nMessage-ID: " + parentID + "\r\nSubject: parent\r\n\r\nparent body")
	require.NoError(t, ingest(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-synthetic-parent", "parent-hash", parentRaw, date, slog.Default()))
	var parentDBID int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-synthetic-parent").Scan(&parentDBID))
	require.NoError(t, finishPstThreads(t.Context(), st, src.ID))
	var replyTo sql.NullInt64
	require.NoError(t, st.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE id = ?`, messageID).Scan(&replyTo))
	require.True(t, replyTo.Valid)
	assert.Equal(t, parentDBID, replyTo.Int64)
}

func TestPSTMeetingRequestSynthesizesHeadersFromSourceProperties(t *testing.T) {
	date := time.Date(2024, 2, 3, 10, 30, 0, 0, time.UTC)
	filetime := make([]byte, 8)
	binary.LittleEndian.PutUint64(filetime, uint64(date.Unix()+11644473600)*10000000)
	string8 := func(id uint16, value string) pstlib.Property {
		return pstlib.Property{ID: id, Type: pstlib.PropertyTypeString8, Data: append([]byte(value), 0)}
	}
	msg := &pstlib.Message{Properties: &properties.Appointment{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		string8(0x001a, "IPM.Schedule.Meeting.Request"),
		string8(0x0c1a, "Organizer"),
		string8(0x0c1e, "SMTP"),
		string8(0x0c1f, "organizer@example.test"),
		string8(0x0037, "Meeting subject"),
		string8(0x1000, "Original meeting text"),
		{ID: 0x0039, Type: pstlib.PropertyTypeTime, Data: filetime},
	}}}
	entry := pstreader.ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	require.NoError(t, entry.BodyError)
	require.NoError(t, entry.DateError)
	raw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	parsed, err := msgmime.Parse(raw)
	require.NoError(t, err)
	require.Len(t, parsed.From, 1)
	assert.Equal(t, "organizer@example.test", parsed.From[0].Email)
	assert.Equal(t, "Meeting subject", parsed.Subject)
	assert.Equal(t, date, parsed.Date)
	assert.Contains(t, parsed.BodyText, "Original meeting text")
}
