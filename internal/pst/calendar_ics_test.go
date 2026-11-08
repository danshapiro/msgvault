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
	original := []AttachmentEntry{{Filename: "source.ics", MIMEType: "text/calendar; method=REQUEST", Content: []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//example//EN\r\nBEGIN:VEVENT\r\nUID:example\r\nDTSTAMP:20240301T090000Z\r\nDTSTART:20240301T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")}}
	assert.Nil(t, entry.meetingICSAttachment(original))
	assert.Empty(t, entry.MeetingICSOmission(original))
	entry.SentAt = time.Time{}
	assert.Nil(t, entry.meetingICSAttachment(nil))
	assert.Equal(t, "meeting message timestamp missing", entry.MeetingICSOmission(nil))
	assert.Empty(t, entry.MeetingICSOmission(original))
}

func TestMeetingICSSourceCalendarMustContainUsableEvent(t *testing.T) {
	start := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	entry := &MessageEntry{EntryID: "3", PSTArchiveID: "source", SentAt: start,
		meetingICS: &meetingICSData{start: start, end: start.Add(time.Hour)}}
	for _, tc := range []struct {
		name, contents string
		usable         bool
	}{
		{"empty", "", false},
		{"arbitrary text", "source", false},
		{"truncated calendar", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T090000Z\r\nDTSTART:20240301T100000Z\r\nEND:VEVENT\r\n", false},
		{"mismatched component", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T090000Z\r\nDTSTART:20240301T100000Z\r\nEND:VTODO\r\nEND:VCALENDAR\r\n", false},
		{"calendar without events", "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n", false},
		{"event without UID", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTAMP:20240301T090000Z\r\nDTSTART:20240301T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", false},
		{"event without timestamp", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTART:20240301T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", false},
		{"invalid timestamp", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T250000Z\r\nDTSTART:20240301T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", false},
		{"event without start", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", false},
		{"invalid date", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T090000Z\r\nDTSTART:20241301T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", false},
		{"all day", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T090000Z\r\nDTSTART;VALUE=DATE:20240301\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", true},
		{"multiple calendars", "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\nBEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T090000Z\r\nDTSTART:20240301T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", true},
		{"custom timezone and folding", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T090000Z\r\nDTSTART;TZID=Custom-\r\n Zone:20240301T100000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", true},
		{"long content value", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:one\r\nDTSTAMP:20240301T090000Z\r\nDTSTART:20240301T100000Z\r\nDESCRIPTION:" + strings.Repeat("a", maxExistingCalendarPrefixBytes+10) + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", true},
		{"malformed parameter", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID;X=\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", false},
		{"oversized parameter", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID;X=" + strings.Repeat("a", maxExistingCalendarPrefixBytes) + ":one\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", false},
		{"oversized quoted parameter containing colon", "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID;X=\"http://" + strings.Repeat("a", maxExistingCalendarPrefixBytes) + "\":one\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", false},
		{"excessive nested components", "BEGIN:VCALENDAR\r\n" + strings.Repeat("BEGIN:X\r\n", 65) + strings.Repeat("END:X\r\n", 65) + "END:VCALENDAR\r\n", false},
		{"extra end before nested components", strings.Repeat("END:X\r\n", 65) + "BEGIN:VCALENDAR\r\n" + strings.Repeat("BEGIN:X\r\n", 65), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attachments := []AttachmentEntry{{Filename: "source.ics", MIMEType: "text/calendar", Content: []byte(tc.contents)}}
			generated := entry.meetingICSAttachment(attachments)
			if tc.usable {
				assert.Nil(t, generated)
			} else {
				require.NotNil(t, generated)
				assert.Equal(t, "meeting.ics", generated.Filename)
			}
			assert.Equal(t, []byte(tc.contents), attachments[0].Content)
		})
	}
	entry.SentAt = time.Time{}
	assert.Equal(t, "meeting message timestamp missing", entry.MeetingICSOmission([]AttachmentEntry{{MIMEType: "text/calendar", Content: []byte("broken")}}))
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
