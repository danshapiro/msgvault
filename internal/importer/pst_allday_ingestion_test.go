package importer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	ical "github.com/emersion/go-ical"
	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	msgmime "go.kenn.io/msgvault/internal/mime"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

func TestPSTAllDayCalendarSurvivesIngestionAndReplay(t *testing.T) {
	// UTC+05:30: the local date starts on the preceding UTC date.
	zone := make([]byte, 76)
	zone[0], zone[1] = 2, 1
	binary.LittleEndian.PutUint16(zone[2:], 6)
	binary.LittleEndian.PutUint16(zone[4:], 2)
	binary.LittleEndian.PutUint16(zone[8:], 1)
	zone[10], zone[11] = 2, 1
	binary.LittleEndian.PutUint16(zone[12:], 0x3e)
	binary.LittleEndian.PutUint16(zone[14:], 2)
	binary.LittleEndian.PutUint16(zone[16:], 2000)
	binary.LittleEndian.PutUint32(zone[32:], ^uint32(329)) // -330 minutes
	filetime := func(t time.Time) []byte {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint64(data, uint64(t.Unix()+11644473600)*10000000)
		return data
	}
	str := func(id uint16, s string) pstlib.Property {
		return pstlib.Property{ID: id, Type: pstlib.PropertyTypeString8, Data: append([]byte(s), 0)}
	}
	start := time.Date(2024, 2, 28, 18, 30, 0, 0, time.UTC)
	named := &pstlib.NameToIDMap{NameToID: map[int]int{}}
	for lid, id := range map[int]int{0x820d: 0x8001, 0x820e: 0x8002, 0x8215: 0x8003, 0x825e: 0x8004, 0x825f: 0x8005} {
		named.NameToID[int(pstlib.PropertySetAppointment)<<32|lid] = id
	}
	msg := &pstlib.Message{Identifier: 321, Properties: &properties.Appointment{}, File: &pstlib.File{NameToIDMap: named}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		str(0x001a, "IPM.Schedule.Meeting.Request"), str(0x0037, "All-day meeting"),
		str(0x1000, "Original body calendarsearchable"),
		str(0x007d, "From: Organizer <organizer@example.test>\r\nTo: attendee@example.test\r\n"),
		{ID: 0x0039, Type: pstlib.PropertyTypeTime, Data: filetime(start.Add(-time.Hour))},
		{ID: 0x8001, Type: pstlib.PropertyTypeTime, Data: filetime(start)},
		{ID: 0x8002, Type: pstlib.PropertyTypeTime, Data: filetime(start.Add(24 * time.Hour))},
		{ID: 0x8003, Type: pstlib.PropertyTypeBoolean, Data: []byte{1, 0}},
		{ID: 0x8004, Type: pstlib.PropertyTypeBinary, Data: zone},
		{ID: 0x8005, Type: pstlib.PropertyTypeBinary, Data: zone},
	}}}
	entry := pstreader.ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	require.NoError(t, entry.BodyError)
	entry.PSTArchiveID = "synthetic-archive"
	ordinary := pstreader.AttachmentEntry{Filename: "original.txt", MIMEType: "text/plain", Content: []byte("original attachment")}
	raw, err := pstreader.BuildRFC5322(entry, []pstreader.AttachmentEntry{ordinary})
	require.NoError(t, err)
	parsed, err := msgmime.Parse(raw)
	require.NoError(t, err)
	parts := make(map[string]msgmime.Attachment)
	for _, part := range parsed.Attachments {
		parts[part.Filename] = part
	}
	require.Contains(t, parts, "meeting.ics")
	calendar, err := ical.NewDecoder(bytes.NewReader(parts["meeting.ics"].Content)).Decode()
	require.NoError(t, err)
	require.Len(t, calendar.Events(), 1)
	event := calendar.Events()[0]
	require.NotNil(t, event.Props.Get("DTSTART"))
	require.NotNil(t, event.Props.Get("DTEND"))
	assert.Equal(t, "20240229", event.Props.Get("DTSTART").Value)
	assert.Equal(t, "DATE", event.Props.Get("DTSTART").Params.Get("VALUE"))
	assert.Equal(t, "20240301", event.Props.Get("DTEND").Value)
	assert.Equal(t, ordinary.Content, parts["original.txt"].Content)
	var facts struct {
		Properties []struct {
			ID    uint16 `json:"property_id"`
			Value []byte `json:"value_base64"`
		} `json:"named_properties"`
	}
	require.Contains(t, parts, "pst-meeting-properties.json")
	require.NoError(t, json.Unmarshal(parts["pst-meeting-properties.json"].Content, &facts))
	zoneCount := 0
	for _, p := range facts.Properties {
		if p.ID == 0x8004 || p.ID == 0x8005 {
			zoneCount++
			assert.Equal(t, zone, p.Value)
		}
	}
	assert.Equal(t, 2, zoneCount)
	st := openIntegrationStore(t)
	src, err := st.GetOrCreateSource("pst", "owner@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	for range 2 {
		require.NoError(t, pstMessageIngester(nil)(t.Context(), st, src.ID, src.Identifier, dir, nil, "pst-synthetic-archive-321", "hash", raw, time.Time{}, slog.Default()))
	}
	var id, count int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-synthetic-archive-321").Scan(&id))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`, src.ID, "pst-synthetic-archive-321").Scan(&count))
	assert.Equal(t, int64(1), count)
	results, total, err := st.SearchMessages("calendarsearchable", 0, 10)
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
