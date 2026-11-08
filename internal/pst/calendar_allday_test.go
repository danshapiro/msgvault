package pst

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	msgmime "go.kenn.io/msgvault/internal/mime"
)

// tzDefinition produces one display rule, using the layout specified by MS-OXOCAL.
func tzDefinition(bias, daylightBias int32, daylightMonth, daylightWeek, standardMonth, standardWeek uint16) []byte {
	data := make([]byte, 10+66)
	data[0], data[1] = 2, 1
	binary.LittleEndian.PutUint16(data[2:], 6)
	binary.LittleEndian.PutUint16(data[4:], 2)
	binary.LittleEndian.PutUint16(data[8:], 1)
	rule := data[10:]
	rule[0], rule[1] = 2, 1
	binary.LittleEndian.PutUint16(rule[2:], 0x3e)
	binary.LittleEndian.PutUint16(rule[4:], 2)
	binary.LittleEndian.PutUint16(rule[6:], 2000)
	binary.LittleEndian.PutUint32(rule[22:], uint32(bias))
	binary.LittleEndian.PutUint32(rule[30:], uint32(daylightBias))
	// SYSTEMTIME: year, month, weekday (Sunday), occurrence, hour, minute, second, milliseconds.
	binary.LittleEndian.PutUint16(rule[36:], standardMonth)
	binary.LittleEndian.PutUint16(rule[40:], standardWeek)
	if standardMonth != 0 {
		binary.LittleEndian.PutUint16(rule[42:], 2)
	}
	binary.LittleEndian.PutUint16(rule[52:], daylightMonth)
	binary.LittleEndian.PutUint16(rule[56:], daylightWeek)
	if daylightMonth != 0 {
		binary.LittleEndian.PutUint16(rule[58:], 2)
	}
	return data
}

func historicalTimeZones(first, second []byte, secondYear uint16) []byte {
	combined := append([]byte(nil), first...)
	binary.LittleEndian.PutUint16(combined[8:], 2)
	binary.LittleEndian.PutUint16(combined[10+4:], 0)
	combined = append(combined, second[10:]...)
	binary.LittleEndian.PutUint16(combined[10+66+6:], secondYear)
	return combined
}

func allDayTestMessage(start, end time.Time, startTZ, endTZ []byte) *pstlib.Message {
	const startID, endID, subtypeID, startTZID, endTZID = 0x8001, 0x8002, 0x8003, 0x8004, 0x8005
	named := map[int]int{}
	for lid, id := range map[int]int{0x820d: startID, 0x820e: endID, 0x8215: subtypeID, 0x825e: startTZID, 0x825f: endTZID} {
		named[int(pstlib.PropertySetAppointment)<<32|lid] = id
	}
	filetime := func(t time.Time) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, uint64(t.Unix()+11644473600)*10000000)
		return b
	}
	props := []pstlib.Property{
		{ID: 0x001a, Type: pstlib.PropertyTypeString8, Data: []byte("IPM.Schedule.Meeting.Request\x00")},
		{ID: 0x0037, Type: pstlib.PropertyTypeString8, Data: []byte("Example all-day meeting\x00")},
		{ID: 0x0039, Type: pstlib.PropertyTypeTime, Data: filetime(start.Add(-time.Hour))},
		{ID: startID, Type: pstlib.PropertyTypeTime, Data: filetime(start)},
		{ID: endID, Type: pstlib.PropertyTypeTime, Data: filetime(end)},
		{ID: subtypeID, Type: pstlib.PropertyTypeBoolean, Data: []byte{1, 0}},
	}
	if startTZ != nil {
		props = append(props, pstlib.Property{ID: startTZID, Type: pstlib.PropertyTypeBinary, Data: startTZ})
	}
	if endTZ != nil {
		props = append(props, pstlib.Property{ID: endTZID, Type: pstlib.PropertyTypeBinary, Data: endTZ})
	}
	return &pstlib.Message{Properties: &properties.Appointment{}, File: &pstlib.File{NameToIDMap: &pstlib.NameToIDMap{NameToID: named}}, PropertyContext: &pstlib.PropertyContext{Properties: props}}
}

func TestAllDayMeetingProducesCalendarAcrossTimeZonesAndDST(t *testing.T) {
	fixed := tzDefinition(-330, 0, 0, 0, 0, 0)
	pacific := tzDefinition(480, -60, 3, 2, 11, 1)
	sydney := tzDefinition(-600, -60, 10, 1, 4, 1)
	historicalPacific := historicalTimeZones(tzDefinition(480, -60, 4, 1, 10, 5), pacific, 2007)
	for _, tc := range []struct {
		name               string
		start, end         time.Time
		zone               []byte
		wantStart, wantEnd string
	}{
		{"fixed offset east of UTC", time.Date(2024, 2, 28, 18, 30, 0, 0, time.UTC), time.Date(2024, 2, 29, 18, 30, 0, 0, time.UTC), fixed, "20240229", "20240301"},
		{"daylight saving transition", time.Date(2024, 3, 9, 8, 0, 0, 0, time.UTC), time.Date(2024, 3, 11, 7, 0, 0, 0, time.UTC), pacific, "20240309", "20240311"},
		{"standard time transition", time.Date(2024, 11, 2, 7, 0, 0, 0, time.UTC), time.Date(2024, 11, 4, 8, 0, 0, 0, time.UTC), pacific, "20241102", "20241104"},
		{"southern hemisphere", time.Date(2024, 4, 5, 13, 0, 0, 0, time.UTC), time.Date(2024, 4, 7, 14, 0, 0, 0, time.UTC), sydney, "20240406", "20240408"},
		{"historical rule before 2007", time.Date(2006, 2, 20, 8, 0, 0, 0, time.UTC), time.Date(2006, 2, 21, 8, 0, 0, 0, time.UTC), historicalPacific, "20060220", "20060221"},
		{"historical rule after 2007", time.Date(2008, 3, 20, 7, 0, 0, 0, time.UTC), time.Date(2008, 3, 21, 7, 0, 0, 0, time.UTC), historicalPacific, "20080320", "20080321"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := allDayTestMessage(tc.start, tc.end, tc.zone, tc.zone)
			entry := ExtractMessage(msg, "Inbox")
			require.NotNil(t, entry)
			require.NoError(t, entry.BodyError)
			entry.EntryID, entry.PSTArchiveID = "entry", "source"
			raw, err := BuildRFC5322(entry, nil)
			require.NoError(t, err)
			parsed, err := msgmime.Parse(raw)
			require.NoError(t, err)
			require.Len(t, parsed.Attachments, 2)
			assert.Equal(t, "meeting.ics", parsed.Attachments[0].Filename)
			assert.Equal(t, "pst-meeting-properties.json", parsed.Attachments[1].Filename)
			assert.Contains(t, string(parsed.Attachments[1].Content), base64.StdEncoding.EncodeToString(tc.zone))
			assert.Contains(t, string(parsed.Attachments[0].Content), "DTSTART;VALUE=DATE:"+tc.wantStart+"\r\n")
			assert.Contains(t, string(parsed.Attachments[0].Content), "DTEND;VALUE=DATE:"+tc.wantEnd+"\r\n")
		})
	}
}

func TestAllDayMeetingRejectsAmbiguousYearBoundary(t *testing.T) {
	zone := historicalTimeZones(tzDefinition(0, 0, 0, 0, 0, 0), tzDefinition(-60, 0, 0, 0, 0, 0), 2007)
	start := time.Date(2006, 12, 31, 23, 0, 0, 0, time.UTC)
	entry := ExtractMessage(allDayTestMessage(start, start.Add(24*time.Hour), zone, zone), "Inbox")
	require.NotNil(t, entry)
	entry.EntryID, entry.PSTArchiveID = "entry", "source"
	assert.NotEmpty(t, entry.MeetingICSWarning())
	assert.Nil(t, entry.meetingICSAttachment(nil))
}

func TestAllDayMeetingRejectsDifferentEffectiveAndHistoricalRules(t *testing.T) {
	oldPacific := tzDefinition(480, -60, 4, 1, 10, 5)
	newPacific := tzDefinition(480, -60, 3, 2, 11, 1)
	zone := historicalTimeZones(oldPacific, newPacific, 2007)
	// On this date the old rule is standard time and the effective rule is
	// daylight time; the source does not determine a unique interpretation.
	start := time.Date(2006, 3, 20, 8, 0, 0, 0, time.UTC)
	entry := ExtractMessage(allDayTestMessage(start, start.Add(24*time.Hour), zone, zone), "Inbox")
	require.NotNil(t, entry)
	entry.EntryID, entry.PSTArchiveID = "entry", "source"
	assert.NotEmpty(t, entry.MeetingICSWarning())
	assert.Nil(t, entry.meetingICSAttachment(nil))
}

func TestAllDayMeetingRejectsAmbiguousOrInvalidZones(t *testing.T) {
	start := time.Date(2024, 3, 9, 8, 0, 0, 0, time.UTC)
	end := time.Date(2024, 3, 11, 7, 0, 0, 0, time.UTC)
	pacific := tzDefinition(480, -60, 3, 2, 11, 1)
	bad := append([]byte(nil), pacific...)
	bad[10+4] = 3 // recurrence flag invalid for display zone
	badTransition := append([]byte(nil), pacific...)
	binary.LittleEndian.PutUint16(badTransition[10+36:], 13)
	overflowBias := append([]byte(nil), pacific...)
	binary.LittleEndian.PutUint32(overflowBias[10+22:], 0x80000000)
	binary.LittleEndian.PutUint32(overflowBias[10+26:], 0x80000000)
	absolute := append([]byte(nil), pacific...)
	binary.LittleEndian.PutUint16(absolute[10+34:], 2024)
	for _, tc := range []struct {
		name   string
		st, et []byte
		shift  time.Duration
	}{
		{"missing start", nil, pacific, 0}, {"missing end", pacific, nil, 0}, {"truncated", pacific[:30], pacific[:30], 0},
		{"invalid flags", bad, bad, 0}, {"conflicting zones", pacific, tzDefinition(0, 0, 0, 0, 0, 0), 0},
		{"invalid transition", badTransition, badTransition, 0}, {"absolute transition", absolute, absolute, 0},
		{"overflowing bias", overflowBias, overflowBias, 0},
		{"not midnight", pacific, pacific, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := ExtractMessage(allDayTestMessage(start.Add(tc.shift), end, tc.st, tc.et), "Inbox")
			require.NotNil(t, entry)
			entry.EntryID, entry.PSTArchiveID = "entry", "source"
			assert.NotEmpty(t, entry.MeetingICSWarning())
			assert.Nil(t, entry.meetingICSAttachment(nil))
		})
	}
}

func TestRecurringAllDayMeetingStillWarns(t *testing.T) {
	start := time.Date(2024, 3, 9, 8, 0, 0, 0, time.UTC)
	zone := tzDefinition(480, -60, 3, 2, 11, 1)
	msg := allDayTestMessage(start, start.Add(24*time.Hour), zone, zone)
	msg.File.NameToIDMap.NameToID[int(pstlib.PropertySetAppointment)<<32|0x8223] = 0x8006
	msg.PropertyContext.Properties = append(msg.PropertyContext.Properties, pstlib.Property{ID: 0x8006, Type: pstlib.PropertyTypeBoolean, Data: []byte{1, 0}})
	entry := ExtractMessage(msg, "Inbox")
	require.NotNil(t, entry)
	entry.EntryID, entry.PSTArchiveID = "entry", "source"
	assert.NotEmpty(t, entry.MeetingICSWarning())
	assert.Nil(t, entry.meetingICSAttachment(nil))
}
