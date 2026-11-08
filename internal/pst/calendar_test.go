package pst

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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
	var values map[string]any
	require.NoError(t, json.Unmarshal(got.Content, &values))
	assert.Equal(t, "2024-01-02T15:30:00Z", values["start_utc"])
	assert.Equal(t, "2024-01-02T16:30:00Z", values["end_utc"])
	assert.Equal(t, "Room A", values["location"])
	assert.Equal(t, "Every Tuesday", values["recurrence_pattern"])
	assert.Len(t, values["named_properties"], 4)
	assert.NotContains(t, values, "name_id_streams") // synthetic source has no PST reader

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
	var roundtrip map[string]any
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

func TestMeetingNamedPropertiesPreserveOriginalBytesAndNameIDStreams(t *testing.T) {
	f, err := Open(filepath.Join("testdata", "support.pst"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, f.Close()) })
	// Values include an unknown ID, an inline value with padding, a Unicode
	// value and a packed variable multivalue. No source identity is inferred.
	items := []pstlib.Property{
		{ID: 0x9001, Type: pstlib.PropertyTypeInteger16, Data: []byte{1, 2, 3, 4}},
		{ID: 0x9002, Type: pstlib.PropertyTypeString, Data: []byte{'A', 0, 0, 0}},
		{ID: 0x9003, Type: pstlib.PropertyTypeMultipleBinary, Data: []byte{1, 0, 0, 0, 8, 0, 0, 0, 0xff, 0xfe}},
	}
	msg := &pstlib.Message{File: f.pstFile, PropertyContext: &pstlib.PropertyContext{Properties: items}}
	raw, streams, err := preserveNamedProperties(msg)
	require.NoError(t, err)
	require.NotNil(t, streams)
	assert.NotEmpty(t, streams.GUID)
	assert.NotEmpty(t, streams.Entry)
	assert.Zero(t, len(streams.Entry)%8)
	require.Len(t, raw, len(items))
	for i, item := range items {
		assert.Equal(t, item.ID, raw[i].ID)
		assert.Equal(t, uint16(item.Type), raw[i].Type)
		assert.Equal(t, item.Data, raw[i].Value)
		assert.Equal(t, "inline", raw[i].Storage)
	}
	encoded, err := json.Marshal(struct {
		Properties []rawNamedProperty `json:"named_properties"`
		Streams    *rawNameIDStreams  `json:"name_id_streams"`
	}{raw, streams})
	require.NoError(t, err)
	var decoded struct {
		Properties []rawNamedProperty `json:"named_properties"`
		Streams    *rawNameIDStreams  `json:"name_id_streams"`
	}
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, raw, decoded.Properties)
	assert.Equal(t, streams, decoded.Streams)
}

func TestMeetingNamedPropertyReadFailureIsFatal(t *testing.T) {
	msg := &pstlib.Message{Properties: &properties.Appointment{}, PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{
		{ID: 0x001a, Type: pstlib.PropertyTypeString8, Data: []byte("IPM.Schedule.Meeting.Request\x00")},
		{ID: 0x8001, Type: pstlib.PropertyTypeBinary, HNID: 123},
	}}}
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	require.ErrorContains(t, entry.BodyError, "property 0x8001 has no source reader")
}

func TestMeetingNamedPropertyRealHeapBytesSurviveMIME(t *testing.T) {
	f, err := Open(filepath.Join("testdata", "support.pst"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, f.Close()) })
	var source *pstlib.Message
	var property pstlib.Property
	stop := errors.New("found heap-backed named property")
	err = f.WalkFolders(func(_ FolderEntry, folder *pstlib.Folder) error {
		if folder.MessageCount == 0 {
			return nil
		}
		iter, err := folder.GetMessageIterator()
		if err != nil {
			return fmt.Errorf("open fixture folder messages: %w", err)
		}
		for iter.Next() {
			msg := iter.Value()
			for _, prop := range msg.PropertyContext.Properties {
				if prop.ID >= 0x8000 && prop.HNID != 0 && len(prop.Data) == 0 {
					source, property = msg, prop
					return stop
				}
			}
		}
		return iter.Err()
	})
	require.ErrorIs(t, err, stop, "fixture must contain a real heap-backed named property")
	reader, err := source.PropertyContext.GetPropertyReader(property.ID, source.LocalDescriptors)
	require.NoError(t, err)
	require.NotNil(t, reader.HeapOnNodeReader)
	require.Positive(t, reader.Size())
	expected := make([]byte, reader.Size())
	n, readErr := reader.ReadAt(expected, 0)
	require.Equal(t, len(expected), n)
	if !errors.Is(readErr, io.EOF) {
		require.NoError(t, readErr)
	}
	attachment, err := meetingCalendarAttachment(source)
	require.NoError(t, err)
	require.NotNil(t, attachment)
	raw, err := BuildRFC5322(&MessageEntry{CalendarAttachment: attachment}, nil)
	require.NoError(t, err)
	parsed, err := msgmime.Parse(raw)
	require.NoError(t, err)
	require.Len(t, parsed.Attachments, 1)
	var data struct {
		Properties []rawNamedProperty `json:"named_properties"`
		Streams    *rawNameIDStreams  `json:"name_id_streams"`
	}
	require.NoError(t, json.Unmarshal(parsed.Attachments[0].Content, &data))
	require.NotNil(t, data.Streams)
	assert.NotEmpty(t, data.Streams.Entry)
	found := false
	for _, got := range data.Properties {
		if got.ID == property.ID {
			found = true
			assert.Equal(t, uint16(property.Type), got.Type)
			assert.Equal(t, "heap_or_subnode", got.Storage)
			assert.Equal(t, expected, got.Value)
		}
	}
	assert.True(t, found, "the real named property must appear in the attachment")
}
