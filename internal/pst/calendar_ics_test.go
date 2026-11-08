package pst

import (
	"bufio"
	"bytes"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingICSAttachmentEscapesAndFoldsUTF8(t *testing.T) {
	start := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	entry := &MessageEntry{
		EntryID: "17", PSTArchiveID: "archive-one",
		SentAt:     start.Add(-time.Hour),
		Subject:    strings.Repeat("日本語", 18) + "; semicolon, comma\\slash\r\nATTENDEE:injected@example.test",
		BodyText:   "Original line one\r\nline two",
		meetingICS: &meetingICSData{start: start, end: start.Add(time.Hour), location: "Room A; east, wing"},
	}
	att := entry.meetingICSAttachment(nil)
	require.NotNil(t, att)
	assert.Equal(t, "meeting.ics", att.Filename)
	assert.Equal(t, "text/calendar", att.MIMEType)
	for line := range bytes.SplitSeq(att.Content, []byte("\r\n")) {
		assert.LessOrEqual(t, len(line), 75)
	}
	unfolded := strings.ReplaceAll(string(att.Content), "\r\n ", "")
	reader := textproto.NewReader(bufio.NewReader(strings.NewReader(unfolded)))
	var logical []string
	for {
		line, err := reader.ReadLine()
		if err != nil {
			break
		}
		logical = append(logical, line)
	}
	joined := strings.Join(logical, "\n")
	assert.Contains(t, joined, "DTSTART:20240301T100000Z")
	assert.Contains(t, joined, "DTEND:20240301T110000Z")
	assert.Contains(t, joined, `LOCATION:Room A\; east\, wing`)
	assert.Contains(t, joined, `DESCRIPTION:Original line one\nline two`)
	assert.Contains(t, joined, `\; semicolon\, comma\\slash\nATTENDEE:injected@example.test`)
	for _, line := range logical {
		assert.False(t, strings.HasPrefix(line, "ATTENDEE:"))
	}
	assert.True(t, bytes.Equal(att.Content, entry.meetingICSAttachment(nil).Content))
	entry.PSTArchiveID = "archive-two"
	assert.NotEqual(t, att.Content, entry.meetingICSAttachment(nil).Content)
}

func TestMeetingICSRespectsSourceCalendarAndMissingInputs(t *testing.T) {
	start := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	entry := &MessageEntry{EntryID: "3", PSTArchiveID: "source", SentAt: start,
		meetingICS: &meetingICSData{start: start, end: start.Add(time.Hour)}}
	original := []AttachmentEntry{{Filename: "source.ics", MIMEType: "text/calendar; method=REQUEST", Content: []byte("source")}}
	assert.Nil(t, entry.meetingICSAttachment(original))
	assert.Empty(t, entry.MeetingICSOmission(original))
	entry.SentAt = time.Time{}
	assert.Nil(t, entry.meetingICSAttachment(nil))
	assert.Equal(t, "meeting message timestamp missing", entry.MeetingICSOmission(nil))
	assert.Empty(t, entry.MeetingICSOmission(original))
}

func TestMeetingICSUsesValidSourceGlobalIdentity(t *testing.T) {
	globalID := make([]byte, 40)
	copy(globalID, []byte{0x04, 0x00, 0x00, 0x00, 0x82, 0x00, 0xe0, 0x00, 0x74, 0xc5, 0xb7, 0x10, 0x1a, 0x82, 0xe0, 0x08})
	require.True(t, validGlobalObjectID(globalID))
	invalid := append([]byte(nil), globalID...)
	invalid[36] = 1 // Declared payload extends beyond the property.
	assert.False(t, validGlobalObjectID(invalid))
	start := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	entry := &MessageEntry{EntryID: "3", PSTArchiveID: "first", SentAt: start,
		meetingICS: &meetingICSData{start: start, end: start.Add(time.Hour), globalID: globalID}}
	first := entry.meetingICSAttachment(nil)
	require.NotNil(t, first)
	entry.PSTArchiveID = "second"
	second := entry.meetingICSAttachment(nil)
	require.NotNil(t, second)
	assert.Equal(t, first.Content, second.Content, "the same source event identity stays stable across archives")
}
