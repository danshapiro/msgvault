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
	location     string
	globalID     []byte
	unsafeReason string
}

// readMeetingICSData declines calendar generation for recurring and all-day
// events until their recurrence or time-zone semantics can be interpreted.
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
	for _, flag := range []struct {
		set pstlib.PropertySet
		lid int
		typ pstlib.PropertyType
	}{
		{pstlib.PropertySetAppointment, 0x8215, pstlib.PropertyTypeBoolean},   // all-day
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
	for _, line := range []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//msgvault//PST import//EN", "METHOD:PUBLISH", "BEGIN:VEVENT",
		fmt.Sprintf("UID:%x@pst.msgvault", uid),
		"DTSTAMP:" + msg.calendarTimestamp().Format("20060102T150405Z"),
		"DTSTART:" + msg.meetingICS.start.Format("20060102T150405Z"),
		"DTEND:" + msg.meetingICS.end.Format("20060102T150405Z"),
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
