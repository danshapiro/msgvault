package importer

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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

func TestPSTGeneratedCalendarSurvivesIngestionAndReplay(t *testing.T) {
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	filetime := func(value time.Time) []byte {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint64(data, uint64(value.Unix()+11644473600)*10000000)
		return data
	}
	str := func(id uint16, value string) pstlib.Property {
		return pstlib.Property{ID: id, Type: pstlib.PropertyTypeString8, Data: append([]byte(value), 0)}
	}
	start := time.Date(2024, time.March, 4, 15, 30, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	sent := start.Add(-time.Hour)
	named := &pstlib.NameToIDMap{NameToID: map[int]int{
		int(pstlib.PropertySetAppointment)<<32 | 0x820d: 0x8001,
		int(pstlib.PropertySetAppointment)<<32 | 0x820e: 0x8002,
		int(pstlib.PropertySetAppointment)<<32 | 0x8208: 0x8003,
	}}
	msg := &pstlib.Message{Identifier: 321, Properties: &properties.Appointment{}, File: &pstlib.File{NameToIDMap: named}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		str(0x001a, "IPM.Schedule.Meeting.Request"),
		str(0x0037, "Synthetic meeting"),
		str(0x1000, "Original body searchablemeeting"),
		str(0x007d, "From: Organizer <organizer@example.test>\r\nTo: attendee@example.test\r\nDate: Mon, 04 Mar 2024 14:30:00 +0000\r\n"),
		{ID: 0x0039, Type: pstlib.PropertyTypeTime, Data: filetime(sent)},
		{ID: 0x8001, Type: pstlib.PropertyTypeTime, Data: filetime(start)},
		{ID: 0x8002, Type: pstlib.PropertyTypeTime, Data: filetime(end)},
		str(0x8003, "Conference Room"),
		{ID: 0x8004, Type: pstlib.PropertyTypeBinary, Data: []byte{0, 1, 0xfe, 0xff}},
	}}}
	entry := pstreader.ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	require.NoError(t, entry.BodyError)
	require.NoError(t, entry.DateError)
	entry.PSTArchiveID = "synthetic-archive"
	ordinary := fixtureMeetingTestAttachment(t)
	ordinaryAttachments := []pstreader.AttachmentEntry{ordinary}
	raw, err := pstreader.BuildRFC5322(entry, ordinaryAttachments)
	require.NoError(t, err)
	before, err := msgmime.Parse(raw)
	require.NoError(t, err)
	require.True(t, before.HasCalendarPart)
	require.Len(t, before.From, 1)
	assert.Equal(t, "organizer@example.test", before.From[0].Email)
	assert.Contains(t, before.BodyText, "Original body searchablemeeting")
	parts := map[string]msgmime.Attachment{}
	for _, a := range before.Attachments {
		parts[a.Filename] = a
	}
	require.Contains(t, parts, ordinary.Filename)
	assert.Equal(t, ordinary.Content, parts[ordinary.Filename].Content)
	require.Contains(t, parts, "meeting.ics")
	assert.Equal(t, "text/calendar", parts["meeting.ics"].ContentType)
	ics := string(parts["meeting.ics"].Content)
	assert.Contains(t, ics, "BEGIN:VCALENDAR\r\n")
	assert.Contains(t, ics, "BEGIN:VEVENT\r\n")
	assert.Contains(t, ics, "DTSTART:20240304T153000Z\r\n")
	assert.Contains(t, ics, "DTEND:20240304T163000Z\r\n")
	assert.Contains(t, ics, "LOCATION:Conference Room\r\n")
	assert.Contains(t, ics, "SUMMARY:Synthetic meeting\r\n")
	assert.NotContains(t, ics, "RRULE:")
	assert.NotContains(t, ics, "ATTENDEE")
	uid := calendarTestUID(ics)
	require.NotEmpty(t, uid)
	require.Contains(t, parts, "pst-meeting-properties.json")
	var facts struct {
		Properties []struct {
			ID    uint16 `json:"property_id"`
			Value []byte `json:"value_base64"`
		} `json:"named_properties"`
	}
	require.NoError(t, json.Unmarshal(parts["pst-meeting-properties.json"].Content, &facts))
	require.Len(t, facts.Properties, 4)
	assert.Equal(t, []byte{0, 1, 0xfe, 0xff}, facts.Properties[3].Value)

	// The same archive and entry must give a stable UID, while an otherwise
	// identical entry from another archive must not collide.
	secondRaw, err := pstreader.BuildRFC5322(entry, ordinaryAttachments)
	require.NoError(t, err)
	second, err := msgmime.Parse(secondRaw)
	require.NoError(t, err)
	assert.Equal(t, uid, calendarTestAttachmentUID(second.Attachments))
	entry.PSTArchiveID = "other-synthetic-archive"
	otherRaw, err := pstreader.BuildRFC5322(entry, nil)
	require.NoError(t, err)
	other, err := msgmime.Parse(otherRaw)
	require.NoError(t, err)
	otherUID := calendarTestAttachmentUID(other.Attachments)
	require.NotEmpty(t, otherUID)
	assert.NotEqual(t, uid, otherUID)

	// Replay with the original source identity must retain one message and
	// preserve all three attachment contents on disk.
	dir := t.TempDir()
	for range 2 {
		require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-synthetic-archive-321", "hash", raw, time.Time{}, slog.Default()))
	}
	var id, count int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-synthetic-archive-321").Scan(&id))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-synthetic-archive-321").Scan(&count))
	assert.Equal(t, int64(1), count)
	body, err := st.GetMessageBodyText(id)
	require.NoError(t, err)
	assert.Contains(t, body, "Original body searchablemeeting")
	results, total, err := st.SearchMessages("searchablemeeting", 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, results, 1)
	assert.Equal(t, id, results[0].ID)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 3)
	for _, ref := range refs {
		part, ok := parts[ref.Filename]
		require.True(t, ok)
		stored, err := os.ReadFile(filepath.Join(dir, ref.StoragePath))
		require.NoError(t, err)
		assert.Equal(t, part.Content, stored)
	}
}

func calendarTestAttachmentUID(attachments []msgmime.Attachment) string {
	for _, item := range attachments {
		if item.Filename == "meeting.ics" {
			return calendarTestUID(string(item.Content))
		}
	}
	return ""
}

func calendarTestUID(body string) string {
	body = strings.ReplaceAll(body, "\r\n ", "")
	for line := range strings.SplitSeq(body, "\r\n") {
		if value, ok := strings.CutPrefix(line, "UID:"); ok {
			return value
		}
	}
	return ""
}

// Use an existing attachment read by the actual PST attachment iterator so
// generated calendar data is exercised alongside a real source attachment.
func fixtureMeetingTestAttachment(t *testing.T) pstreader.AttachmentEntry {
	t.Helper()
	f, err := pstreader.Open(filepath.Join(pstTestdataDir, "support.pst"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, f.Close()) })
	stop := errors.New("found fixture attachment")
	var result pstreader.AttachmentEntry
	err = f.WalkFolders(func(_ pstreader.FolderEntry, folder *pstlib.Folder) error {
		if folder.MessageCount == 0 {
			return nil
		}
		iter, err := folder.GetMessageIterator()
		if err != nil {
			return fmt.Errorf("open fixture folder: %w", err)
		}
		for iter.Next() {
			attachments, err := pstreader.ReadAttachments(iter.Value(), 0)
			if err != nil {
				return fmt.Errorf("read fixture attachments: %w", err)
			}
			for _, att := range attachments {
				if att.Filename != "" && !strings.ContainsAny(att.Filename, `/\`) && len(att.Content) > 0 {
					result = att
					return stop
				}
			}
		}
		return iter.Err()
	})
	require.ErrorIs(t, err, stop, "fixture must contain a readable source attachment")
	return result
}

func TestPSTCalendarRequiresRealDatesAndUnambiguousOccurrence(t *testing.T) {
	filetime := func(value time.Time) []byte {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint64(data, uint64(value.Unix()+11644473600)*10000000)
		return data
	}
	str := func(id uint16, value string) pstlib.Property {
		return pstlib.Property{ID: id, Type: pstlib.PropertyTypeString8, Data: append([]byte(value), 0)}
	}
	start := time.Date(2024, time.March, 4, 15, 30, 0, 0, time.UTC)
	named := &pstlib.NameToIDMap{NameToID: map[int]int{
		int(pstlib.PropertySetAppointment)<<32 | 0x820d: 0x8001,
		int(pstlib.PropertySetAppointment)<<32 | 0x820e: 0x8002,
		int(pstlib.PropertySetAppointment)<<32 | 0x8223: 0x8003,
		int(pstlib.PropertySetAppointment)<<32 | 0x8215: 0x8004,
		int(pstlib.PropertySetAppointment)<<32 | 0x8232: 0x8005,
	}}
	base := []pstlib.Property{
		str(0x001a, "IPM.Schedule.Meeting.Request"),
		str(0x1000, "Actual original message body"),
		{ID: 0x0039, Type: pstlib.PropertyTypeTime, Data: filetime(start.Add(-time.Hour))},
		{ID: 0x8001, Type: pstlib.PropertyTypeTime, Data: filetime(start)},
		{ID: 0x8002, Type: pstlib.PropertyTypeTime, Data: filetime(start.Add(time.Hour))},
	}
	for _, tc := range []struct {
		name  string
		skip  uint16
		extra *pstlib.Property
	}{
		{name: "missing start", skip: 0x8001},
		{name: "missing end", skip: 0x8002},
		{name: "missing source timestamp", skip: 0x0039},
		{name: "recurrence not decoded", extra: &pstlib.Property{ID: 0x8003, Type: pstlib.PropertyTypeBoolean, Data: []byte{1, 0, 0, 0}}},
		{name: "all-day timezone not decoded", extra: &pstlib.Property{ID: 0x8004, Type: pstlib.PropertyTypeBoolean, Data: []byte{1, 0, 0, 0}}},
		{name: "recurrence description without rules", extra: &pstlib.Property{ID: 0x8005, Type: pstlib.PropertyTypeString8, Data: []byte("Every Tuesday\x00")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var props []pstlib.Property
			for _, p := range base {
				if p.ID != tc.skip {
					props = append(props, p)
				}
			}
			if tc.extra != nil {
				props = append(props, *tc.extra)
			}
			msg := &pstlib.Message{Identifier: 500, Properties: &properties.Appointment{}, File: &pstlib.File{NameToIDMap: named}, PropertyContext: &pstlib.PropertyContext{Properties: props}}
			entry := pstreader.ExtractMessage(msg, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.HeaderError)
			require.NoError(t, entry.BodyError)
			entry.PSTArchiveID = "synthetic-archive"
			raw, err := pstreader.BuildRFC5322(entry, nil)
			require.NoError(t, err)
			parsed, err := msgmime.Parse(raw)
			require.NoError(t, err)
			assert.Contains(t, parsed.BodyText, "Actual original message body")
			assert.False(t, parsed.HasCalendarPart, "do not invent a timestamp or recurrence")
			for _, part := range parsed.Attachments {
				assert.NotEqual(t, "meeting.ics", part.Filename)
			}
		})
	}
	other := &pstlib.Message{Properties: &properties.Appointment{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{str(0x001a, "IPM.Appointment")}}}
	assert.Nil(t, pstreader.ExtractMessage(other, "Calendar"))
}
