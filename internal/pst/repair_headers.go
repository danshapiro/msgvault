package pst

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// RepairTransportHeader fixes a malformed leading PST transport header while
// leaving the stored MIME body, including its boundary, byte-for-byte intact.
// A matching preservation header makes a partially completed repair retryable.
func RepairTransportHeader(entry *MessageEntry, raw []byte) ([]byte, bool, error) {
	if entry == nil || entry.TransportHeaders == "" {
		return nil, false, errors.New("PST transport headers unavailable")
	}
	separator := bytes.Index(raw, []byte("\r\n\r\n"))
	if separator < 0 {
		return nil, false, errors.New("stored message has no MIME header separator")
	}
	oldHeader := string(raw[:separator])
	safeHeader, malformed := validTransportHeaders(oldHeader)
	if !malformed {
		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			return nil, false, fmt.Errorf("parse stored headers: %w", err)
		}
		values := parsed.Header["X-Msgvault-Original-Transport-Headers"]
		if len(values) != 1 {
			return nil, false, errors.New("stored message is not a matching PST header repair")
		}
		decoded, err := base64.RawURLEncoding.DecodeString(strings.Join(strings.Fields(values[0]), ""))
		if err != nil || !bytes.Equal(decoded, []byte(entry.TransportHeaders)) {
			return nil, false, errors.New("stored repair metadata does not match PST headers")
		}
		return raw, false, nil
	}
	// The faulty builder copied the original leading line verbatim. Requiring
	// an exact match binds the repair to this PST entry rather than replacing a
	// different malformed message that happens to have the same node number.
	firstSourceLine, _, _ := strings.Cut(entry.TransportHeaders, "\n")
	firstSourceLine = strings.TrimSuffix(firstSourceLine, "\r")
	firstStoredLine, _, _ := strings.Cut(oldHeader, "\r\n")
	if firstSourceLine == "" || firstSourceLine != firstStoredLine {
		return nil, false, errors.New("stored leading header does not match PST entry")
	}
	// Refuse collisions instead of allowing source-supplied data to impersonate
	// the metadata that authorizes a retry.
	if parsed, err := mail.ReadMessage(strings.NewReader(safeHeader + "\r\n")); err != nil {
		return nil, false, fmt.Errorf("parse recovered headers: %w", err)
	} else if len(parsed.Header["X-Msgvault-Original-Transport-Headers"]) != 0 || len(parsed.Header["X-Msgvault-Original-Stored-Headers"]) != 0 {
		return nil, false, errors.New("stored transport metadata header conflicts with repair")
	}
	var out bytes.Buffer
	out.WriteString(strings.ReplaceAll(safeHeader, "\n", "\r\n"))
	writeEncodedBytesHeader(&out, "X-Msgvault-Original-Transport-Headers", []byte(entry.TransportHeaders))
	// The stored snapshot may contain malformed lines not present in the PST
	// property. Preserve that entire header too rather than discard those bytes.
	writeEncodedBytesHeader(&out, "X-Msgvault-Original-Stored-Headers", []byte(oldHeader))
	out.Write(raw[separator+2:])
	return out.Bytes(), true, nil
}
