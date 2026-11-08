package pst

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
)

// A source attachment may be arbitrarily large or malformed. A bounded parse
// cannot affect preservation: the original attachment is always kept.
const maxExistingCalendarBytes = 16 << 20
const maxExistingCalendarDepth = 64
const maxExistingCalendarPrefixBytes = 4096

// usableExistingCalendar requires a complete VCALENDAR with at least one
// VEVENT that has an identity, a creation timestamp, and a valid start date.
// DTSTART is checked syntactically without resolving custom Outlook TZIDs.
func usableExistingCalendar(content []byte) bool {
	if len(content) == 0 || len(content) > maxExistingCalendarBytes {
		return false
	}
	content = bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf})
	if !calendarDepthWithinLimit(content) {
		return false
	}
	return parseExistingCalendar(content)
}

func validCalendarStart(prop *ical.Prop) bool {
	kind := strings.ToUpper(prop.Params.Get("VALUE"))
	value := prop.Value
	switch {
	case kind == "DATE" || kind == "" && len(value) == 8:
		_, err := time.Parse("20060102", value)
		return err == nil
	case kind == "" || kind == "DATE-TIME":
		layout := "20060102T150405"
		if strings.HasSuffix(value, "Z") {
			layout += "Z"
		}
		_, err := time.Parse(layout, value)
		return err == nil
	default:
		return false
	}
}

// Bound nesting before the third-party parser recursively visits components.
// Unfolding first also catches BEGIN names split across continuation lines.
func calendarDepthWithinLimit(content []byte) bool {
	unfolded := bytes.ReplaceAll(content, []byte("\r\n "), nil)
	unfolded = bytes.ReplaceAll(unfolded, []byte("\r\n\t"), nil)
	unfolded = bytes.ReplaceAll(unfolded, []byte("\n "), nil)
	unfolded = bytes.ReplaceAll(unfolded, []byte("\n\t"), nil)
	depth := 0
	for line := range bytes.SplitSeq(unfolded, []byte("\n")) {
		// go-ical builds parameter values incrementally; bound the prefix
		// before the first unquoted colon to avoid quadratic work. Long
		// property values, including DESCRIPTION, are still allowed.
		quoted, colon := false, -1
		for i, c := range line {
			if i >= maxExistingCalendarPrefixBytes {
				return false
			}
			if c == '"' {
				quoted = !quoted
			} else if c == ':' && !quoted {
				colon = i
				break
			}
		}
		if colon < 0 && len(bytes.TrimSpace(line)) != 0 {
			return false
		}
		name, _, found := bytes.Cut(line, []byte(":"))
		if !found {
			continue
		}
		name, _, _ = bytes.Cut(name, []byte(";"))
		switch {
		case bytes.EqualFold(name, []byte("BEGIN")):
			depth++
			if depth > maxExistingCalendarDepth {
				return false
			}
		case bytes.EqualFold(name, []byte("END")):
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// go-ical can panic on malformed parameters. Isolate that parser boundary so
// invalid source attachments cannot interrupt an import.
func parseExistingCalendar(content []byte) (usable bool) {
	defer func() {
		if recover() != nil {
			usable = false
		}
	}()
	decoder := ical.NewDecoder(bytes.NewReader(content))
	for {
		calendar, err := decoder.Decode()
		if errors.Is(err, io.EOF) {
			return usable
		}
		if err != nil {
			return false
		}
		for _, event := range calendar.Events() {
			uid := event.Props.Get("UID")
			stamp := event.Props.Get("DTSTAMP")
			start := event.Props.Get("DTSTART")
			if uid == nil || strings.TrimSpace(uid.Value) == "" || stamp == nil || start == nil {
				continue
			}
			if _, err := time.Parse("20060102T150405Z", stamp.Value); err != nil {
				continue
			}
			if validCalendarStart(start) {
				usable = true
			}
		}
	}
}
