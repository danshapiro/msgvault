package pst

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
)

// BuildRFC5322 constructs raw RFC 5322/MIME bytes from a MessageEntry and its
// attachments.
//
// Strategy:
//  1. If TransportHeaders is non-empty, use those verbatim as the message
//     headers, filling missing threading headers from MAPI and stripping
//     existing MIME content headers we will replace.
//  2. Otherwise, synthesize headers from MAPI properties.
//  3. Build the body as multipart/alternative when both text and HTML are
//     present, or as a simple text/plain or text/html part when only one
//     exists.
//  4. When attachments are present, wrap the body in multipart/mixed.
func BuildRFC5322(msg *MessageEntry, attachments []AttachmentEntry) ([]byte, error) {
	// headerBuf holds all RFC 5322 header lines (without the trailing blank line).
	// bodyBuf holds the MIME body (MIME-Version, Content-Type, blank line, body).
	var headerBuf, bodyBuf bytes.Buffer

	// Write message-identifying headers.
	if msg.TransportHeaders != "" {
		present := writeTransportHeaders(&headerBuf, msg.TransportHeaders, msg)
		writeThreadingHeaders(&headerBuf, msg, present)
	} else {
		writeSynthesizedHeaders(&headerBuf, msg)
	}
	if err := writeUnresolvedIdentityHeaders(&headerBuf, msg); err != nil {
		return nil, err
	}
	if err := writeUnresolvedRecipientHeaders(&headerBuf, msg); err != nil {
		return nil, err
	}

	hasText := msg.BodyText != ""
	hasHTML := msg.BodyHTML != ""
	hasAtts := len(attachments) > 0

	switch {
	case hasAtts:
		// Wrap body and attachments in multipart/mixed.
		mw := multipart.NewWriter(&bodyBuf)
		fmt.Fprintf(&headerBuf, "MIME-Version: 1.0\r\nContent-Type: multipart/mixed;\r\n\tboundary=%q\r\n\r\n", mw.Boundary())

		// Body sub-part.
		bh := make(textproto.MIMEHeader)
		if hasText && hasHTML {
			// Build the multipart/alternative sub-part into a buffer so we know
			// the boundary before writing the outer part header.
			var innerBuf bytes.Buffer
			altW := multipart.NewWriter(&innerBuf)
			writeTextPart(altW, msg.BodyText)
			writeHTMLPart(altW, msg.BodyHTML)
			_ = altW.Close()

			bh.Set("Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, altW.Boundary()))
			pw, _ := mw.CreatePart(bh)
			_, _ = pw.Write(innerBuf.Bytes())
		} else if hasHTML {
			bh.Set("Content-Type", "text/html; charset=utf-8")
			bh.Set("Content-Transfer-Encoding", "quoted-printable")
			pw, _ := mw.CreatePart(bh)
			writeQP(pw, msg.BodyHTML)
		} else {
			bh.Set("Content-Type", "text/plain; charset=utf-8")
			bh.Set("Content-Transfer-Encoding", "quoted-printable")
			pw, _ := mw.CreatePart(bh)
			writeQP(pw, msg.BodyText)
		}

		// Attachment parts.
		for i := range attachments {
			att := &attachments[i]
			ah := make(textproto.MIMEHeader)
			ct := att.MIMEType
			if ct == "" {
				ct = "application/octet-stream"
			} else if _, _, err := mime.ParseMediaType(ct); err != nil {
				ct = "application/octet-stream"
			}
			fname := sanitizeFilename(att.Filename)
			if fname != "" {
				ah.Set("Content-Type", mime.FormatMediaType(ct, map[string]string{"name": fname}))
			} else {
				ah.Set("Content-Type", ct)
			}
			if att.ContentID != "" {
				cid := sanitizeContentID(att.ContentID)
				ah.Set("Content-Id", "<"+cid+">")
				ah.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", fname))
			} else {
				ah.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fname))
			}
			ah.Set("Content-Transfer-Encoding", "base64")
			pw, _ := mw.CreatePart(ah)
			enc := base64.NewEncoder(base64.StdEncoding, pw)
			_, _ = enc.Write(att.Content)
			_ = enc.Close()
		}
		_ = mw.Close()

	case hasText && hasHTML:
		// multipart/alternative with no attachments.
		mw := multipart.NewWriter(&bodyBuf)
		fmt.Fprintf(&headerBuf, "MIME-Version: 1.0\r\nContent-Type: multipart/alternative;\r\n\tboundary=%q\r\n\r\n", mw.Boundary())
		writeTextPart(mw, msg.BodyText)
		writeHTMLPart(mw, msg.BodyHTML)
		_ = mw.Close()

	case hasHTML:
		headerBuf.WriteString("MIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		writeQP(&bodyBuf, msg.BodyHTML)

	default:
		// text/plain only, or empty body.
		headerBuf.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		writeQP(&bodyBuf, msg.BodyText)
	}

	return append(headerBuf.Bytes(), bodyBuf.Bytes()...), nil
}

// writeTextPart writes a text/plain MIME part to mw.
func writeTextPart(mw *multipart.Writer, text string) {
	th := make(textproto.MIMEHeader)
	th.Set("Content-Type", "text/plain; charset=utf-8")
	th.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, _ := mw.CreatePart(th)
	writeQP(pw, text)
}

// writeHTMLPart writes a text/html MIME part to mw.
func writeHTMLPart(mw *multipart.Writer, html string) {
	th := make(textproto.MIMEHeader)
	th.Set("Content-Type", "text/html; charset=utf-8")
	th.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, _ := mw.CreatePart(th)
	writeQP(pw, html)
}

// writeTransportHeaders writes the original transport headers to buf,
// stripping any existing MIME content headers that we will replace.
func writeTransportHeaders(buf *bytes.Buffer, headers string, msg *MessageEntry) map[string]bool {
	headers = strings.ReplaceAll(headers, "\r\n", "\n")
	headers = strings.ReplaceAll(headers, "\r", "\n")
	present := make(map[string]bool)
	recipientFields := make(map[string][]string)
	var field []string
	flush := func() {
		if len(field) == 0 {
			return
		}
		name, value, _ := strings.Cut(field[0], ":")
		name = strings.ToLower(name)
		switch name {
		case "mime-version", "content-type", "content-transfer-encoding":
			return
		case "to", "cc", "bcc":
			value += " " + strings.Join(field[1:], " ")
			recipientFields[name] = append(recipientFields[name], strings.TrimSpace(value))
			return
		case "from":
			fullValue := strings.TrimSpace(value + " " + strings.Join(field[1:], " "))
			addresses, err := mail.ParseAddressList(fullValue)
			if err != nil || len(addresses) == 0 || !strings.Contains(addresses[0].Address, "@") {
				if fallbackAuthor(msg) != nil {
					writeHeader(buf, "X-Msgvault-Original-From", sanitizeHeaderValue(fullValue))
					return
				}
			} else {
				present[name] = true
			}
		case "subject":
			if strings.TrimSpace(value+strings.Join(field[1:], "")) != "" {
				present[name] = true
			} else if msg.Subject != "" {
				return
			}
		case "sender":
			if strings.TrimSpace(value+strings.Join(field[1:], "")) != "" {
				present[name] = true
			}
		case "message-id", "in-reply-to", "references":
			if strings.TrimSpace(value+strings.Join(field[1:], "")) == "" {
				return
			}
			present[name] = true
		}
		for _, line := range field {
			buf.WriteString(line)
			buf.WriteString("\r\n")
		}
	}
	for line := range strings.SplitSeq(headers, "\n") {
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			field = append(field, line)
			continue
		}
		flush()
		field = []string{line}
	}
	flush()
	if !present["from"] {
		if author := fallbackAuthor(msg); author != nil {
			writeHeader(buf, "From", author.String())
		}
	}
	if !present["subject"] && strings.TrimSpace(msg.Subject) != "" {
		writeHeader(buf, "Subject", mime.QEncoding.Encode("utf-8", sanitizeHeaderValue(msg.Subject)))
	}
	if !present["sender"] {
		writeRepresentingSender(buf, msg)
	}
	writeRecipientHeaders(buf, msg, recipientFields)
	return present
}

func fallbackAuthor(msg *MessageEntry) *mail.Address {
	name, email := msg.AuthorName, msg.AuthorEmail
	if email == "" {
		name, email = msg.SenderName, msg.SenderEmail
	}
	email = sanitizeHeaderValue(strings.TrimSpace(email))
	parsed, err := mail.ParseAddress(email)
	if err != nil || !strings.Contains(parsed.Address, "@") {
		return nil
	}
	return &mail.Address{Name: sanitizeHeaderValue(name), Address: parsed.Address}
}

func writeRepresentingSender(buf *bytes.Buffer, msg *MessageEntry) {
	if msg.AuthorEmail == "" || msg.SenderEmail == "" || strings.EqualFold(msg.AuthorEmail, msg.SenderEmail) {
		return
	}
	email := sanitizeHeaderValue(strings.TrimSpace(msg.SenderEmail))
	parsed, err := mail.ParseAddress(email)
	if err == nil && strings.Contains(parsed.Address, "@") {
		writeHeader(buf, "Sender", (&mail.Address{Name: sanitizeHeaderValue(msg.SenderName), Address: parsed.Address}).String())
	}
}

// writeRecipientHeaders merges transport and recipient-table addresses by
// role. Existing Bcc/CC values are retained and addresses are deduplicated
// only within the same role.
func writeRecipientHeaders(buf *bytes.Buffer, msg *MessageEntry, fields map[string][]string) {
	for _, role := range []struct {
		name    string
		role    int32
		display string
	}{{"To", 1, msg.DisplayTo}, {"Cc", 2, msg.DisplayCc}, {"Bcc", 3, msg.DisplayBcc}} {
		original := fields[strings.ToLower(role.name)]
		var addresses []*mail.Address
		hasUnresolved := false
		seen := make(map[string]bool)
		invalid := false
		for _, value := range original {
			parsed, err := mail.ParseAddressList(value)
			if err != nil {
				invalid = true
				continue
			}
			for _, address := range parsed {
				if !strings.Contains(address.Address, "@") {
					invalid = true
					continue
				}
				key := strings.ToLower(address.Address)
				if !seen[key] {
					addresses = append(addresses, address)
					seen[key] = true
				}
			}
		}
		for _, recipient := range msg.Recipients {
			if recipient.Role != role.role {
				continue
			}
			if recipient.Unresolved {
				hasUnresolved = true
				continue
			}
			key := strings.ToLower(recipient.Email)
			if !seen[key] {
				addresses = append(addresses, &mail.Address{Name: sanitizeHeaderValue(recipient.Name), Address: sanitizeHeaderValue(recipient.Email)})
				seen[key] = true
			}
		}
		if invalid {
			if len(addresses) == 0 && !hasUnresolved {
				for _, value := range original {
					writeHeader(buf, role.name, sanitizeHeaderValue(value))
				}
				continue
			}
			for _, value := range original {
				writeHeader(buf, "X-Msgvault-Original-"+role.name, sanitizeHeaderValue(value))
			}
		}
		if len(addresses) == 0 {
			if len(original) == 0 && role.display != "" && !hasUnresolved {
				writeHeader(buf, role.name, formatDisplayList(role.display))
			}
			continue
		}
		buf.WriteString(role.name + ": ")
		for i, address := range addresses {
			if i > 0 {
				buf.WriteString(",\r\n\t")
			}
			buf.WriteString(address.String())
		}
		buf.WriteString("\r\n")
	}
}

// writeEncodedMetadataHeader preserves source values without letting their
// contents create headers. Folding keeps even long values within line limits.
func writeEncodedMetadataHeader(buf *bytes.Buffer, header string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	buf.WriteString(header + ": ")
	for len(encoded) > 72 {
		buf.WriteString(encoded[:72])
		buf.WriteString("\r\n\t")
		encoded = encoded[72:]
	}
	buf.WriteString(encoded + "\r\n")
	return nil
}

func writeUnresolvedIdentityHeaders(buf *bytes.Buffer, msg *MessageEntry) error {
	for _, item := range []struct {
		header   string
		identity *UnresolvedIdentity
	}{
		{"X-Msgvault-Unresolved-Sender", msg.UnresolvedSender},
		{"X-Msgvault-Unresolved-Author", msg.UnresolvedAuthor},
	} {
		if item.identity == nil {
			continue
		}
		identity := item.identity
		if err := writeEncodedMetadataHeader(buf, item.header, struct {
			Name        string `json:"name"`
			AddressType string `json:"address_type"`
			Address     string `json:"address"`
			SMTP        string `json:"smtp"`
		}{identity.Name, identity.AddressType, identity.RawAddress, identity.RawSMTP}); err != nil {
			return fmt.Errorf("encode %s: %w", item.header, err)
		}
	}
	return nil
}

// Keep unresolved MAPI facts in raw MIME without placing an unverified value
// in an email-address field. Base64url makes controls and line breaks safe.
func writeUnresolvedRecipientHeaders(buf *bytes.Buffer, msg *MessageEntry) error {
	for _, recipient := range msg.Recipients {
		if !recipient.Unresolved {
			continue
		}
		name := strings.TrimSpace(strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, recipient.Name))
		if name != "" {
			// The complete name remains in the encoded header below.
			runes := []rune(name)
			if len(runes) > 256 {
				name = string(runes[:256])
			}
			role := map[int32]string{1: "To", 2: "Cc", 3: "Bcc"}[recipient.Role]
			writeHeader(buf, "X-Msgvault-Unresolved-Recipient-Name", mime.QEncoding.Encode("utf-8", role+": "+name))
		}
		if err := writeEncodedMetadataHeader(buf, "X-Msgvault-Unresolved-Recipient", struct {
			Role        int32  `json:"role"`
			Name        string `json:"name"`
			AddressType string `json:"address_type"`
			Address     string `json:"address"`
			SMTP        string `json:"smtp"`
		}{recipient.Role, recipient.Name, recipient.AddressType, recipient.RawAddress, recipient.RawSMTP}); err != nil {
			return fmt.Errorf("encode unresolved recipient: %w", err)
		}
	}
	return nil
}

// writeSynthesizedHeaders writes RFC 5322 headers synthesized from MAPI
// properties when TransportMessageHeaders is absent.
func writeSynthesizedHeaders(buf *bytes.Buffer, msg *MessageEntry) {
	if author := fallbackAuthor(msg); author != nil {
		writeHeader(buf, "From", author.String())
		writeRepresentingSender(buf, msg)
	} else if msg.UnresolvedSender == nil && msg.UnresolvedAuthor == nil {
		writeHeader(buf, "From", formatAddr(msg.SenderName, msg.SenderEmail))
	}
	writeRecipientHeaders(buf, msg, nil)

	t := msg.FallbackDate(time.Now())
	if !t.IsZero() {
		writeHeader(buf, "Date", t.Format(time.RFC1123Z))
	}

	if msg.Subject != "" {
		writeHeader(buf, "Subject", mime.QEncoding.Encode("utf-8", sanitizeHeaderValue(msg.Subject)))
	}

	writeThreadingHeaders(buf, msg, nil)
	writeHeader(buf, "X-Msgvault-Source", "pst")
	writeHeader(buf, "X-Msgvault-Synthesized", "true")
}

// writeThreadingHeaders fills only fields not supplied by transport headers.
func writeThreadingHeaders(buf *bytes.Buffer, msg *MessageEntry, present map[string]bool) {
	for _, field := range []struct{ name, value string }{
		{"Message-Id", msg.MessageID}, {"In-Reply-To", msg.InReplyTo}, {"References", msg.References},
	} {
		value := strings.TrimSpace(sanitizeHeaderValue(field.value))
		if present[strings.ToLower(field.name)] || value == "" {
			continue
		}
		if field.name != "References" && !strings.HasPrefix(value, "<") {
			value = "<" + value + ">"
		}
		writeHeader(buf, field.name, value)
	}
}

func writeHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

// sanitizeHeaderValue strips CR and LF characters to prevent header injection.
func sanitizeHeaderValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// sanitizeFilename strips path components and dangerous characters from
// attachment filenames sourced from PST data. Handles both Unix and Windows
// path separators since PST data originates on Windows.
func sanitizeFilename(name string) string {
	// Normalize Windows backslash separators before calling filepath.Base.
	name = strings.ReplaceAll(name, `\`, "/")
	name = filepath.Base(name)
	if name == "." {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
}

// sanitizeContentID strips characters that could break the Content-Id
// angle-bracket wrapper.
func sanitizeContentID(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '\r', '\n':
			return -1
		default:
			return r
		}
	}, s)
}

// formatAddr formats a display name + email as "Name <email>" or just email.
func formatAddr(name, email string) string {
	name = sanitizeHeaderValue(name)
	email = sanitizeHeaderValue(email)
	if name == "" && email == "" {
		return ""
	}
	if name == "" {
		return email
	}
	if email == "" {
		return mime.QEncoding.Encode("utf-8", name)
	}
	return fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", name), email)
}

// formatDisplayList converts a semicolon-separated PST display list to a
// comma-separated header value. PST DisplayTo/Cc/Bcc fields contain display
// names only (not email addresses), so we emit them without angle brackets.
func formatDisplayList(display string) string {
	parts := strings.Split(display, ";")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(sanitizeHeaderValue(p))
		if p != "" {
			cleaned = append(cleaned, mime.QEncoding.Encode("utf-8", p))
		}
	}
	return strings.Join(cleaned, ", ")
}

// writeQP writes s as quoted-printable text to dst.
// Lines longer than 76 characters are soft-wrapped with "=\r\n".
// Trailing whitespace before line breaks is encoded per RFC 2045 §6.7.
func writeQP(dst interface{ Write(p []byte) (int, error) }, s string) {
	const maxLine = 76
	var line strings.Builder

	encodeTrailingWS := func() {
		str := line.String()
		if len(str) == 0 {
			return
		}
		last := str[len(str)-1]
		if last == ' ' || last == '\t' {
			line.Reset()
			line.WriteString(str[:len(str)-1])
			fmt.Fprintf(&line, "=%02X", last)
		}
	}

	flush := func(soft bool) {
		if !soft {
			encodeTrailingWS()
		}
		if soft {
			_, _ = dst.Write([]byte(line.String() + "=\r\n"))
		} else {
			_, _ = dst.Write([]byte(line.String() + "\r\n"))
		}
		line.Reset()
	}

	for _, b := range []byte(s) {
		var encoded string
		switch {
		case b == '\r':
			continue
		case b == '\n':
			flush(false)
			continue
		case b == '=':
			encoded = "=3D"
		case b < 32 || b > 126:
			encoded = fmt.Sprintf("=%02X", b)
		default:
			encoded = string(rune(b))
		}

		if line.Len()+len(encoded) > maxLine {
			flush(true)
		}
		line.WriteString(encoded)
	}
	if line.Len() > 0 {
		flush(false)
	}
}
