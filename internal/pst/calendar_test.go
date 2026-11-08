package pst

import (
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	msgmime "go.kenn.io/msgvault/internal/mime"
)

func TestMeetingCalendarAttachment(t *testing.T) {
	const startID, endID, locationID, patternID = 0x8001, 0x8002, 0x8003, 0x8004
	named := &pstlib.NameToIDMap{NameToID: map[int]int{
		int(pstlib.PropertySetAppointment)<<32 | 0x820d: startID,
		int(pstlib.PropertySetAppointment)<<32 | 0x820e: endID,
		int(pstlib.PropertySetAppointment)<<32 | 0x8208: locationID,
		int(pstlib.PropertySetAppointment)<<32 | 0x8232: patternID,
	}}
	filetime := func(t time.Time) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, uint64(t.Unix()+11644473600)*10000000)
		return b
	}
	start := time.Date(2024, 1, 2, 15, 30, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	msg := &pstlib.Message{Properties: &properties.Appointment{}, File: &pstlib.File{NameToIDMap: named}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x001a, Type: pstlib.PropertyTypeString8, Data: []byte("IPM.Schedule.Meeting.Request\x00")},
		{ID: startID, Type: pstlib.PropertyTypeTime, Data: filetime(start)},
		{ID: endID, Type: pstlib.PropertyTypeTime, Data: filetime(end)},
		{ID: locationID, Type: pstlib.PropertyTypeString8, Data: []byte("Room A\x00")},
		{ID: patternID, Type: pstlib.PropertyTypeString8, Data: []byte("Every Tuesday\x00")},
	}}}
	got, err := meetingCalendarAttachment(msg)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "pst-meeting-properties.json", got.Filename)
	assert.Equal(t, "application/json", got.MIMEType)
	var values map[string]string
	require.NoError(t, json.Unmarshal(got.Content, &values))
	assert.Equal(t, map[string]string{"start_utc": "2024-01-02T15:30:00Z", "end_utc": "2024-01-02T16:30:00Z", "location": "Room A", "recurrence_pattern": "Every Tuesday"}, values)

	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.NoError(t, entry.HeaderError)
	require.NoError(t, entry.BodyError)
	require.NotNil(t, entry.CalendarAttachment)
	original := []byte("ordinary attachment")
	raw, err := BuildRFC5322(entry, []AttachmentEntry{{Filename: "ordinary.txt", MIMEType: "text/plain", Content: original}})
	require.NoError(t, err)
	parsed, err := msgmime.Parse(raw)
	require.NoError(t, err)
	require.Len(t, parsed.Attachments, 2)
	assert.Equal(t, "ordinary.txt", parsed.Attachments[0].Filename)
	assert.Equal(t, original, parsed.Attachments[0].Content)
	assert.Equal(t, "pst-meeting-properties.json", parsed.Attachments[1].Filename)
	var roundtrip map[string]string
	require.NoError(t, json.Unmarshal(parsed.Attachments[1].Content, &roundtrip))
	assert.Equal(t, values, roundtrip)
}

func TestMeetingCalendarAttachmentMissingAndInvalid(t *testing.T) {
	msg := &pstlib.Message{PropertyContext: &pstlib.PropertyContext{}}
	got, err := meetingCalendarAttachment(msg)
	require.NoError(t, err)
	assert.Nil(t, got)
	msg.File = &pstlib.File{NameToIDMap: &pstlib.NameToIDMap{NameToID: map[int]int{int(pstlib.PropertySetAppointment)<<32 | 0x820d: 0x8001}}}
	got, err = meetingCalendarAttachment(msg)
	require.NoError(t, err)
	assert.Nil(t, got)
	msg.PropertyContext.Properties = []pstlib.Property{{ID: 0x8001, Type: pstlib.PropertyTypeTime, Data: []byte{1}}}
	got, err = meetingCalendarAttachment(msg)
	require.Error(t, err)
	assert.Nil(t, got)
	msg.Properties = &properties.Appointment{}
	msg.PropertyContext.Properties = append(msg.PropertyContext.Properties, pstlib.Property{ID: 0x001a, Type: pstlib.PropertyTypeString8, Data: []byte("IPM.Schedule.Meeting.Request\x00")})
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.ErrorContains(t, entry.BodyError, "read meeting start")
}

func TestMeetingCalendarAttachmentRejectsUnreadableText(t *testing.T) {
	msg := &pstlib.Message{File: &pstlib.File{NameToIDMap: &pstlib.NameToIDMap{NameToID: map[int]int{
		int(pstlib.PropertySetAppointment)<<32 | 0x8208: 0x8001,
	}}}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x8001, Type: pstlib.PropertyTypeString, Data: []byte{1}},
	}}}
	got, err := meetingCalendarAttachment(msg)
	require.ErrorContains(t, err, "read meeting location")
	assert.Nil(t, got)
}
