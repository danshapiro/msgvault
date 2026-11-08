package pst

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
)

// meetingCalendarAttachment preserves selected source properties as facts; it
// does not infer an iCalendar UID, recurrence rule, organizer, or timezone.
func meetingCalendarAttachment(msg *pstlib.Message) (*AttachmentEntry, error) {
	if msg.File == nil || msg.File.NameToIDMap == nil || msg.PropertyContext == nil {
		return nil, nil //nolint:nilnil // No named-property map means no calendar facts are available.
	}
	type calendarFacts struct {
		StartUTC          string `json:"start_utc,omitempty"`
		EndUTC            string `json:"end_utc,omitempty"`
		Location          string `json:"location,omitempty"`
		RecurrencePattern string `json:"recurrence_pattern,omitempty"`
	}
	var facts calendarFacts
	for _, item := range []struct {
		lid   int
		label string
		date  *string
		text  *string
	}{
		{0x820d, "start", &facts.StartUTC, nil},
		{0x820e, "end", &facts.EndUTC, nil},
		{0x8208, "location", nil, &facts.Location},
		{0x8232, "recurrence pattern", nil, &facts.RecurrencePattern},
	} {
		mapped, err := msg.File.NameToIDMap.GetPropertyID(item.lid, pstlib.PropertySetAppointment)
		if errors.Is(err, pstlib.ErrNameToIDMapKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("map meeting %s property: %w", item.label, err)
		}
		if mapped < 0 || mapped > 0xffff {
			return nil, fmt.Errorf("map meeting %s property: invalid ID %d", item.label, mapped)
		}
		id := uint16(mapped)
		if item.date != nil {
			value, err := messageDate(msg, id, nil)
			if err != nil {
				return nil, fmt.Errorf("read meeting %s: %w", item.label, err)
			}
			if !value.IsZero() {
				*item.date = value.UTC().Format(time.RFC3339Nano)
			}
		} else {
			value, err := propertyString(msg, id, "")
			if err != nil {
				return nil, fmt.Errorf("read meeting %s: %w", item.label, err)
			}
			*item.text = value
		}
	}
	if facts == (calendarFacts{}) {
		return nil, nil //nolint:nilnil // An empty set of facts needs no attachment.
	}
	data, err := json.Marshal(facts)
	if err != nil {
		return nil, fmt.Errorf("encode meeting properties: %w", err)
	}
	return &AttachmentEntry{Filename: "pst-meeting-properties.json", MIMEType: "application/json", Content: data}, nil
}
