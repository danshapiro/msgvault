package pst

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
)

// meetingICSData contains only fields whose meaning is known. Original named
// properties remain in the separate preservation attachment.
type meetingICSData struct {
	start, end   time.Time
	allDay       bool
	location     string
	globalID     []byte
	unsafeReason string
}

// readMeetingICSData declines calendar generation when recurrence or time-zone
// semantics cannot be interpreted safely.
func readMeetingICSData(msg *pstlib.Message) *meetingICSData {
	data := &meetingICSData{}
	if msg.File == nil || msg.File.NameToIDMap == nil || msg.PropertyContext == nil {
		data.unsafeReason = "calendar property mapping unavailable"
		return data
	}
	lookup := func(set pstlib.PropertySet, lid int) (pstlib.Property, bool) {
		id, err := msg.File.NameToIDMap.GetPropertyID(lid, set)
		if err != nil || id < 0 || id > 0xffff {
			return pstlib.Property{}, false
		}
		prop, err := msg.PropertyContext.GetPropertyByID(uint16(id))
		return prop, err == nil
	}
	if prop, ok := lookup(pstlib.PropertySetAppointment, 0x8215); ok {
		value, err := bodyPropertyBytes(msg, prop)
		if prop.Type != pstlib.PropertyTypeBoolean || err != nil || len(value) < 1 || len(value) > 4 || value[0] > 1 || len(bytes.Trim(value[1:], "\x00")) != 0 {
			data.unsafeReason = "unreadable all-day property"
			return data
		}
		data.allDay = value[0] == 1
	}
	for _, flag := range []struct {
		set pstlib.PropertySet
		lid int
		typ pstlib.PropertyType
	}{
		{pstlib.PropertySetAppointment, 0x8223, pstlib.PropertyTypeBoolean},   // recurring
		{pstlib.PropertySetMeeting, 0x0005, pstlib.PropertyTypeBoolean},       // recurring
		{pstlib.PropertySetAppointment, 0x8231, pstlib.PropertyTypeInteger32}, // recurrence type
		{pstlib.PropertySetAppointment, 0x8228, pstlib.PropertyTypeTime},      // exception
	} {
		if prop, ok := lookup(flag.set, flag.lid); ok {
			if prop.Type != flag.typ {
				data.unsafeReason = "ambiguous recurrence or all-day properties"
				return data
			}
			value, err := bodyPropertyBytes(msg, prop)
			if err != nil || len(value) == 0 {
				data.unsafeReason = "unreadable recurrence or all-day properties"
				return data
			}
			for _, b := range value {
				if b != 0 {
					data.unsafeReason = "recurrence, exception, or all-day information requires interpretation"
					return data
				}
			}
		}
	}
	if prop, ok := lookup(pstlib.PropertySetAppointment, 0x8216); ok && (prop.HNID != 0 || len(prop.Data) != 0) {
		data.unsafeReason = "recurrence information requires interpretation"
		return data
	}
	if prop, ok := lookup(pstlib.PropertySetAppointment, 0x8232); ok {
		pattern, err := propertyString(msg, prop.ID, "")
		if err != nil || strings.TrimSpace(pattern) != "" {
			data.unsafeReason = "recurrence description requires interpretation"
			return data
		}
	}
	for _, field := range []struct {
		lid  int
		dest *time.Time
	}{{0x820d, &data.start}, {0x820e, &data.end}} {
		prop, ok := lookup(pstlib.PropertySetAppointment, field.lid)
		if !ok {
			data.unsafeReason = "meeting start or end missing"
			return data
		}
		value, err := messageDate(msg, prop.ID, nil)
		if err != nil || value.IsZero() {
			data.unsafeReason = "meeting start or end unreadable"
			return data
		}
		*field.dest = value.UTC()
	}
	if !data.end.After(data.start) || data.start.Year() < 1 || data.start.Year() > 9999 || data.end.Year() < 1 || data.end.Year() > 9999 {
		data.unsafeReason = "meeting start or end invalid"
		return data
	}
	if data.allDay {
		var zones [2][]byte
		for i, lid := range []int{0x825e, 0x825f} {
			prop, ok := lookup(pstlib.PropertySetAppointment, lid)
			if !ok || prop.Type != pstlib.PropertyTypeBinary {
				data.unsafeReason = "all-day time zone unavailable"
				return data
			}
			var err error
			zones[i], err = bodyPropertyBytes(msg, prop)
			if err != nil {
				data.unsafeReason = "all-day time zone unreadable"
				return data
			}
		}
		if !bytes.Equal(zones[0], zones[1]) {
			data.unsafeReason = "all-day time zones differ"
			return data
		}
		zone, ok := parseAllDayTimeZone(zones[0])
		if !ok {
			data.unsafeReason = "all-day time zone unsupported"
			return data
		}
		start, startOK := zone.localMidnight(data.start)
		end, endOK := zone.localMidnight(data.end)
		if !startOK || !endOK || !end.After(start) {
			data.unsafeReason = "all-day local dates unavailable"
			return data
		}
		data.start, data.end = start, end
	}
	if prop, ok := lookup(pstlib.PropertySetAppointment, 0x8208); ok {
		location, err := propertyString(msg, prop.ID, "")
		if err == nil {
			data.location = location
		}
	}
	for _, lid := range []int{0x0023, 0x0003} { // clean GlobalObjectId, then GlobalObjectId
		if prop, ok := lookup(pstlib.PropertySetMeeting, lid); ok && prop.Type == pstlib.PropertyTypeBinary {
			if id, err := bodyPropertyBytes(msg, prop); err == nil && validGlobalObjectID(id) {
				data.globalID = id
				break
			}
		}
	}
	return data
}

// GlobalObjectId begins with the documented Outlook identifier. An arbitrary
// binary property is not a trustworthy event identity.
func validGlobalObjectID(id []byte) bool {
	return len(id) >= 40 && bytes.Equal(id[:16], []byte{0x04, 0x00, 0x00, 0x00, 0x82, 0x00, 0xe0, 0x00, 0x74, 0xc5, 0xb7, 0x10, 0x1a, 0x82, 0xe0, 0x08}) && int64(binary.LittleEndian.Uint32(id[36:40])) == int64(len(id))-40
}

// MeetingICSWarning reports why a recognized meeting request cannot safely
// produce a calendar attachment. The original properties are still retained.
func (msg *MessageEntry) MeetingICSWarning() string {
	if msg.meetingICS == nil {
		return ""
	}
	if msg.meetingICS.unsafeReason != "" {
		return msg.meetingICS.unsafeReason
	}
	if msg.calendarTimestamp().IsZero() {
		return "meeting message timestamp missing"
	}
	if len(msg.meetingICS.globalID) == 0 && (msg.PSTArchiveID == "" || msg.EntryID == "") && strings.TrimSpace(msg.MessageID) == "" {
		return "meeting identity unavailable"
	}
	return ""
}

// MeetingICSOmission reports why no generated calendar will be available,
// except when an original calendar attachment already supplies one.
func (msg *MessageEntry) MeetingICSOmission(attachments []AttachmentEntry) string {
	if hasUsableExistingCalendar(attachments) {
		return ""
	}
	return msg.MeetingICSWarning()
}

func hasUsableExistingCalendar(attachments []AttachmentEntry) bool {
	for _, att := range attachments {
		mediaType, _, err := mime.ParseMediaType(att.MIMEType)
		if err == nil && strings.EqualFold(mediaType, "text/calendar") && usableExistingCalendar(att.Content) {
			return true
		}
	}
	return false
}

func (msg *MessageEntry) calendarTimestamp() time.Time {
	for _, t := range []time.Time{msg.SentAt, msg.CreationTime} {
		if !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 {
			return t.UTC()
		}
	}
	if msg.TransportHeaders != "" {
		m, err := mail.ReadMessage(strings.NewReader(msg.TransportHeaders + "\r\n\r\n"))
		if err == nil {
			t, err := mail.ParseDate(m.Header.Get("Date"))
			if err == nil && t.Year() >= 1 && t.Year() <= 9999 {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

func (msg *MessageEntry) meetingICSAttachment(attachments []AttachmentEntry) *AttachmentEntry {
	if msg.meetingICS == nil || msg.MeetingICSWarning() != "" || hasUsableExistingCalendar(attachments) {
		return nil
	}
	var uidInput []byte
	switch {
	case len(msg.meetingICS.globalID) > 0:
		uidInput = append([]byte("outlook-global-object-id\x00"), msg.meetingICS.globalID...)
	case msg.PSTArchiveID != "" && msg.EntryID != "":
		uidInput = []byte("pst-entry\x00" + msg.PSTArchiveID + "\x00" + msg.EntryID)
	default:
		uidInput = []byte("message-id\x00" + strings.TrimSpace(msg.MessageID))
	}
	uid := sha256.Sum256(uidInput)
	var out bytes.Buffer
	dateStart, dateEnd := "DTSTART:"+msg.meetingICS.start.Format("20060102T150405Z"), "DTEND:"+msg.meetingICS.end.Format("20060102T150405Z")
	if msg.meetingICS.allDay {
		dateStart, dateEnd = "DTSTART;VALUE=DATE:"+msg.meetingICS.start.Format("20060102"), "DTEND;VALUE=DATE:"+msg.meetingICS.end.Format("20060102")
	}
	for _, line := range []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//msgvault//PST import//EN", "METHOD:PUBLISH", "BEGIN:VEVENT",
		fmt.Sprintf("UID:%x@pst.msgvault", uid),
		"DTSTAMP:" + msg.calendarTimestamp().Format("20060102T150405Z"),
		dateStart, dateEnd,
	} {
		writeICSLine(&out, line)
	}
	for _, field := range []struct{ name, value string }{
		{"SUMMARY", msg.Subject}, {"LOCATION", msg.meetingICS.location}, {"DESCRIPTION", msg.BodyText},
	} {
		if field.value != "" {
			writeICSLine(&out, field.name+":"+escapeICSText(field.value))
		}
	}
	writeICSLine(&out, "END:VEVENT")
	writeICSLine(&out, "END:VCALENDAR")
	return &AttachmentEntry{Filename: generatedCalendarFilename(attachments), MIMEType: "text/calendar", Content: out.Bytes()}
}

// Keep the generated calendar distinct from a malformed original with the
// same name. Both are preserved as separate attachments.
func generatedCalendarFilename(attachments []AttachmentEntry) string {
	used := make(map[string]bool, len(attachments))
	for _, att := range attachments {
		used[strings.ToLower(sanitizeFilename(att.Filename))] = true
	}
	name := "meeting.ics"
	for n := 2; used[strings.ToLower(name)]; n++ {
		name = fmt.Sprintf("meeting-%d.ics", n)
	}
	return name
}

func escapeICSText(s string) string {
	s = strings.ToValidUTF8(s, "\ufffd")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			out.WriteString(`\\`)
		case ';':
			out.WriteString(`\;`)
		case ',':
			out.WriteString(`\,`)
		case '\n':
			out.WriteString(`\n`)
		default:
			if r >= 0x20 && r != 0x7f {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

// Fold by UTF-8 octets, preserving complete code points and limiting each
// physical line to 75 octets including the continuation space.
func writeICSLine(out *bytes.Buffer, line string) {
	width := 0
	for len(line) > 0 {
		_, size := utf8.DecodeRuneInString(line)
		if width+size > 75 {
			out.WriteString("\r\n ")
			width = 1
		}
		out.WriteString(line[:size])
		width += size
		line = line[size:]
	}
	out.WriteString("\r\n")
}

// A display TZDEFINITION describes the zone in which a single event's UTC
// timestamps were selected. Absolute one-off transitions remain preserved in
// the original properties and require interpretation.
type allDayTimeZone struct {
	rules     []allDayRule
	effective int
}

type allDayRule struct {
	standardBias, daylightBias int
	year                       int
	standard, daylight         allDayTransition
}

type allDayTransition struct {
	month, weekday, week, hour, minute, second, millis int
}

func parseAllDayTimeZone(data []byte) (allDayTimeZone, bool) {
	var zone allDayTimeZone
	if len(data) < 10 || data[0] != 2 || data[1] != 1 || binary.LittleEndian.Uint16(data[4:]) != 2 {
		return zone, false
	}
	u16 := binary.LittleEndian.Uint16
	keyChars := int(u16(data[6:]))
	if keyChars > 260 {
		return zone, false
	}
	headerLen := 6 + keyChars*2
	if int(u16(data[2:])) != headerLen || len(data) < 4+headerLen {
		return zone, false
	}
	count := int(u16(data[8+keyChars*2:]))
	if count < 1 || count > 1024 || len(data) != 4+headerLen+66*count {
		return zone, false
	}
	parseTransition := func(raw []byte) (allDayTransition, bool) {
		if bytes.Equal(raw, make([]byte, 16)) {
			return allDayTransition{}, true
		}
		// Absolute transition dates need additional historical rules.
		if u16(raw) != 0 {
			return allDayTransition{}, false
		}
		tr := allDayTransition{int(u16(raw[2:])), int(u16(raw[4:])), int(u16(raw[6:])), int(u16(raw[8:])), int(u16(raw[10:])), int(u16(raw[12:])), int(u16(raw[14:]))}
		return tr, tr.month >= 1 && tr.month <= 12 && tr.weekday <= 6 && tr.week >= 1 && tr.week <= 5 && tr.hour <= 23 && tr.minute <= 59 && tr.second <= 59 && tr.millis <= 999
	}
	signed32 := func(raw []byte) int64 {
		value := int64(binary.LittleEndian.Uint32(raw))
		if value >= 1<<31 {
			value -= 1 << 32
		}
		return value
	}
	effective := 0
	for i := range count {
		raw := data[4+headerLen+66*i:][:66]
		flags := u16(raw[4:])
		if raw[0] != 2 || raw[1] != 1 || u16(raw[2:]) != 0x3e || flags != 0 && flags != 2 || !bytes.Equal(raw[8:22], make([]byte, 14)) {
			return allDayTimeZone{}, false
		}
		if flags == 2 {
			effective++
			zone.effective = i
		}
		rule := allDayRule{year: int(u16(raw[6:]))}
		if i > 0 && rule.year <= zone.rules[i-1].year {
			return allDayTimeZone{}, false
		}
		base := signed32(raw[22:])
		standardBias := base + signed32(raw[26:])
		daylightBias := base + signed32(raw[30:])
		if standardBias < -1440 || standardBias > 1440 || daylightBias < -1440 || daylightBias > 1440 {
			return allDayTimeZone{}, false
		}
		rule.standardBias, rule.daylightBias = int(standardBias), int(daylightBias)
		var ok bool
		rule.standard, ok = parseTransition(raw[34:50])
		if !ok {
			return allDayTimeZone{}, false
		}
		rule.daylight, ok = parseTransition(raw[50:66])
		if !ok || (rule.standard.month == 0) != (rule.daylight.month == 0) || rule.standard == rule.daylight && rule.standard.month != 0 {
			return allDayTimeZone{}, false
		}
		zone.rules = append(zone.rules, rule)
	}
	return zone, effective == 1
}

func (zone allDayTimeZone) localMidnight(instant time.Time) (time.Time, bool) {
	result, ok := zone.localTime(instant)
	if !ok {
		return time.Time{}, false
	}
	// MS-OXCICAL exports the effective rule even when the definition also has
	// historical rules. Decline the event when the two interpretations differ.
	effectiveRule := zone.rules[zone.effective]
	effectiveRule.year = 0
	effective := allDayTimeZone{rules: []allDayRule{effectiveRule}}
	current, ok := effective.localTime(instant)
	if !ok || !current.Equal(result) || result.Hour() != 0 || result.Minute() != 0 || result.Second() != 0 || result.Nanosecond() != 0 {
		return time.Time{}, false
	}
	return result, true
}

// localTime evaluates every possible local year near the instant. If a rule
// change at a year boundary makes the result ambiguous, decline it.
func (zone allDayTimeZone) localTime(instant time.Time) (time.Time, bool) {
	var result time.Time
	for year := instant.Year() - 1; year <= instant.Year()+1; year++ {
		if year < 1 || year > 9999 {
			continue
		}
		var rule *allDayRule
		for i := range zone.rules {
			if zone.rules[i].year <= year {
				rule = &zone.rules[i]
			} else {
				break
			}
		}
		if rule == nil {
			continue
		}
		bias := rule.standardBias
		if rule.standard.month != 0 {
			daylight := rule.daylight.inYear(year).Add(time.Duration(rule.standardBias) * time.Minute)
			standard := rule.standard.inYear(year).Add(time.Duration(rule.daylightBias) * time.Minute)
			if daylight.Equal(standard) {
				return time.Time{}, false
			}
			var inDaylight bool
			if daylight.Before(standard) {
				inDaylight = !instant.Before(daylight) && instant.Before(standard)
			} else {
				inDaylight = instant.Before(standard) || !instant.Before(daylight)
			}
			if inDaylight {
				bias = rule.daylightBias
			}
		}
		local := instant.UTC().Add(-time.Duration(bias) * time.Minute)
		if local.Year() != year {
			continue
		}
		if !result.IsZero() && !result.Equal(local) {
			return time.Time{}, false
		}
		result = local
	}
	if result.IsZero() {
		return time.Time{}, false
	}
	return result, true
}

func (tr allDayTransition) inYear(year int) time.Time {
	first := time.Date(year, time.Month(tr.month), 1, tr.hour, tr.minute, tr.second, tr.millis*int(time.Millisecond), time.UTC)
	day := 1 + (tr.weekday-int(first.Weekday())+7)%7 + (tr.week-1)*7
	last := time.Date(year, time.Month(tr.month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > last {
		day -= 7
	}
	return time.Date(year, time.Month(tr.month), day, tr.hour, tr.minute, tr.second, tr.millis*int(time.Millisecond), time.UTC)
}
