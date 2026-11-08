package pst

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"net/mail"
	"strings"
	"testing"
	"testing/quick"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/rotisserie/eris"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func goPSTIteratorErr(cause error) error {
	err := eris.Wrap(cause, "failed to get attachment table context")
	err = eris.Wrap(err, "failed to get attachment table context")
	return eris.Wrap(err, "failed to get attachment count")
}

func TestNoAttachments(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "empty attachment table",
			err:  goPSTIteratorErr(pstlib.ErrTableContextNoRows),
			want: true,
		},
		{
			name: "empty attachment table rewrapped",
			err:  fmt.Errorf("get attachment iterator: %w", goPSTIteratorErr(pstlib.ErrTableContextNoRows)),
			want: true,
		},
		{
			name: "attachment flag unset",
			err:  pstlib.ErrAttachmentsNotFound,
			want: true,
		},
		{
			name: "same text without sentinel",
			err:  goPSTIteratorErr(errors.New("go-pst: there are no rows in this table context")),
			want: false,
		},
		{
			name: "table without columns",
			err:  goPSTIteratorErr(pstlib.ErrTableContextNoColumns),
			want: false,
		},
		{
			name: "missing local descriptor",
			err:  goPSTIteratorErr(eris.Wrap(pstlib.ErrLocalDescriptorNotFound, "failed to find attachment local descriptor")),
			want: false,
		},
		{
			name: "invalid attachment index",
			err:  goPSTIteratorErr(pstlib.ErrAttachmentIndexInvalid),
			want: false,
		},
		{
			name: "size cap",
			err:  errAttachmentTooLarge,
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, noAttachments(tt.err))
		})
	}
}

func TestMessageDate(t *testing.T) {
	const propertyID = 0x0039
	tests := []struct {
		name  string
		ticks uint64
		want  time.Time
	}{
		{"zero is absent", 0, time.Time{}},
		{"unix epoch", 116444736000000000, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"before unix epoch", 116444735990000000, time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"2024 with fraction", 133497882001234567, time.Date(2024, 1, 15, 10, 30, 0, 123456700, time.UTC)},
		{"beyond unix nanoseconds", 2650467744000000000, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, 8)
			binary.LittleEndian.PutUint64(data, tt.ticks)
			msg := &pstlib.Message{PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{{ID: propertyID, Type: pstlib.PropertyTypeTime, Data: data}}}}
			got, err := messageDate(msg, propertyID, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("missing", func(t *testing.T) {
		got, err := messageDate(&pstlib.Message{PropertyContext: &pstlib.PropertyContext{}}, propertyID, nil)
		require.NoError(t, err)
		assert.True(t, got.IsZero())
	})
	t.Run("short data reports error", func(t *testing.T) {
		msg := &pstlib.Message{PropertyContext: &pstlib.PropertyContext{Properties: []pstlib.Property{{ID: propertyID, Type: pstlib.PropertyTypeTime, Data: []byte{1}}}}}
		_, err := messageDate(msg, propertyID, nil)
		assert.Error(t, err)
	})
	t.Run("synthetic unix epoch", func(t *testing.T) {
		var value int64
		got, err := messageDate(&pstlib.Message{}, propertyID, &value)
		require.NoError(t, err)
		assert.Equal(t, time.Unix(0, 0).UTC(), got)
	})
}

func TestFallbackDate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	valid := time.Date(1965, 1, 2, 0, 0, 0, 0, time.UTC)
	entry := &MessageEntry{SentAt: time.Date(1601, 1, 1, 0, 0, 0, 100, time.UTC), ReceivedAt: valid}
	assert.Equal(t, valid, entry.FallbackDate(now))
	entry.SentAt = now.AddDate(1, 0, 0)
	assert.Equal(t, valid, entry.FallbackDate(now))
	entry.ReceivedAt = time.Time{}
	assert.True(t, entry.FallbackDate(now).IsZero())
}

func TestExtractCN(t *testing.T) {
	tests := []struct {
		dn   string
		want string
	}{
		{"/O=CORP/OU=EXCHANGE/CN=RECIPIENTS/CN=JSMITH", "JSMITH"},
		{"/o=Contoso/ou=Exchange/cn=Recipients/cn=jdoe", "jdoe"},
		{"user@example.com", "user@example.com"}, // not a DN
		{"", ""},
	}
	for _, tt := range tests {
		got := extractCN(tt.dn)
		assert.Equal(t, tt.want, got, "extractCN(%q)", tt.dn)
	}
}

func TestIsExchangeDN(t *testing.T) {
	assert.True(t, isExchangeDN("/O=CORP/OU=EXCH/CN=user"), "expected true for /O= DN")
	assert.True(t, isExchangeDN("/o=corp/cn=user"), "expected true for /o= DN")
	assert.False(t, isExchangeDN("user@example.com"), "expected false for SMTP address")
}

func TestBuildRFC5322_SynthesizedHeaders(t *testing.T) {
	assert := assert.New(t)
	msg := &MessageEntry{
		EntryID:     "12345",
		FolderPath:  "Inbox",
		Subject:     "Hello World",
		BodyText:    "This is a test message.",
		SenderName:  "Alice",
		SenderEmail: "alice@example.com",
		DisplayTo:   "Bob",
		MessageID:   "<abc123@example.com>",
		SentAt:      time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}

	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	assert.Contains(s, "From:", "missing From header")
	assert.Contains(s, "alice@example.com", "missing sender email")
	assert.Contains(s, "Subject:", "missing Subject header")
	assert.Contains(s, "Message-Id:", "missing Message-Id header")
	assert.Contains(s, "X-Msgvault-Synthesized: true", "missing X-Msgvault-Synthesized header")
	assert.Contains(s, "text/plain", "missing text/plain content type")
	assert.Contains(s, "This is a test message", "body text not found in output")
}

func TestBuildRFC5322_TransportHeaders(t *testing.T) {
	assert := assert.New(t)
	transportHeaders := "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Test\r\nMessage-ID: <orig@example.com>\r\nDate: Mon, 15 Jan 2024 10:30:00 +0000\r\n"

	msg := &MessageEntry{
		EntryID:          "99",
		TransportHeaders: transportHeaders,
		BodyText:         "Body text here.",
		BodyHTML:         "<p>Body HTML here.</p>",
	}

	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	// Original headers should be present.
	assert.Contains(s, "From: alice@example.com", "missing original From header")
	assert.Contains(s, "Message-ID: <orig@example.com>", "missing original Message-ID header")
	// Should NOT have synthesized header.
	assert.NotContains(s, "X-Msgvault-Synthesized", "should not have X-Msgvault-Synthesized when transport headers present")
	// Both text and HTML → multipart/alternative.
	assert.Contains(s, "multipart/alternative", "expected multipart/alternative for text+html body")
}

func TestBuildRFC5322_MergesMAPIHeadersByRole(t *testing.T) {
	msg := &MessageEntry{
		TransportHeaders: "From: original@example.test\r\nTo: Bob <bob@example.test>\r\nBcc: hidden@example.test\r\n",
		Subject:          "Recovered subject",
		SenderEmail:      "fallback@example.test",
		Recipients: []MessageRecipient{
			{Name: "Duplicate", Email: "BOB@example.test", Role: 1},
			{Name: "Alice", Email: "alice@example.test", Role: 1},
			{Name: "Alice", Email: "alice@example.test", Role: 2},
			{Name: "Secret", Email: "secret@example.test", Role: 3},
		},
		BodyText: "body",
	}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err)
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, "original@example.test", message.Header.Get("From"))
	assert.Equal(t, "Recovered subject", message.Header.Get("Subject"))
	to, err := message.Header.AddressList("To")
	require.NoError(t, err)
	require.Len(t, to, 2)
	assert.Equal(t, "bob@example.test", to[0].Address)
	assert.Equal(t, "alice@example.test", to[1].Address)
	cc, err := message.Header.AddressList("Cc")
	require.NoError(t, err)
	require.Len(t, cc, 1)
	assert.Equal(t, "alice@example.test", cc[0].Address)
	bcc, err := message.Header.AddressList("Bcc")
	require.NoError(t, err)
	require.Len(t, bcc, 2)
	assert.Equal(t, "hidden@example.test", bcc[0].Address)
	assert.Equal(t, "secret@example.test", bcc[1].Address)
}

func TestBuildRFC5322_PreservesUnresolvedRecipient(t *testing.T) {
	recipient, err := makeMessageRecipient(1, "José\r\nBcc: injected@example.test", "EX", "/O=EXAMPLE/CN=JOSE", "")
	require.NoError(t, err)
	require.NotNil(t, recipient)
	msg := &MessageEntry{SenderEmail: "sender@example.test", Recipients: []MessageRecipient{*recipient}, BodyText: "body"}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "\r\nBcc:")
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	encoded := message.Header.Get("X-Msgvault-Unresolved-Recipient")
	require.NotEmpty(t, encoded)
	decoded, err := base64.RawURLEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	require.NoError(t, err)
	var preserved struct {
		Role        int32  `json:"role"`
		Name        string `json:"name"`
		AddressType string `json:"address_type"`
		Address     string `json:"address"`
	}
	require.NoError(t, json.Unmarshal(decoded, &preserved))
	assert.Equal(t, int32(1), preserved.Role)
	assert.Equal(t, "José\r\nBcc: injected@example.test", preserved.Name)
	assert.Equal(t, "EX", preserved.AddressType)
	assert.Equal(t, "/O=EXAMPLE/CN=JOSE", preserved.Address)
	assert.Empty(t, message.Header.Get("To"), "an unresolved display name must not become an email address")
	assert.NotEmpty(t, message.Header.Get("X-Msgvault-Unresolved-Recipient-Name"))
}

func TestBuildRFC5322_ValidAndUnresolvedRecipientsShareRole(t *testing.T) {
	unresolved, err := makeMessageRecipient(2, "Unknown person", "SMTP", "malformed address", "also malformed")
	require.NoError(t, err)
	msg := &MessageEntry{
		TransportHeaders: "From: sender@example.test\r\nCc: existing@example.test\r\n",
		Recipients:       []MessageRecipient{*unresolved, {Name: "New", Email: "new@example.test", Role: 2}},
		BodyText:         "body",
	}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err)
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	cc, err := message.Header.AddressList("Cc")
	require.NoError(t, err)
	require.Len(t, cc, 2)
	assert.Equal(t, "existing@example.test", cc[0].Address)
	assert.Equal(t, "new@example.test", cc[1].Address)
	assert.Contains(t, message.Header.Get("X-Msgvault-Unresolved-Recipient-Name"), "Unknown person")
	encoded := message.Header.Get("X-Msgvault-Unresolved-Recipient")
	decoded, err := base64.RawURLEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	require.NoError(t, err)
	var preserved struct {
		Role    int32  `json:"role"`
		Address string `json:"address"`
		SMTP    string `json:"smtp"`
	}
	require.NoError(t, json.Unmarshal(decoded, &preserved))
	assert.Equal(t, int32(2), preserved.Role)
	assert.Equal(t, "malformed address", preserved.Address)
	assert.Equal(t, "also malformed", preserved.SMTP)
}

func TestBuildRFC5322_ExistingNameOnlyDoesNotBecomeAddress(t *testing.T) {
	msg := &MessageEntry{
		TransportHeaders: "From: sender@example.test\r\nTo: Unresolved Person\r\n",
		Recipients:       []MessageRecipient{{Name: "Unresolved Person", Role: 1, Unresolved: true, AddressType: "EX", RawAddress: "/O=EXAMPLE/CN=PERSON"}},
	}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err)
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, "Unresolved Person", message.Header.Get("X-Msgvault-Original-To"))
	assert.Empty(t, message.Header.Get("To"), "the display name is not a verified email address")
	assert.Contains(t, message.Header.Get("X-Msgvault-Unresolved-Recipient-Name"), "Unresolved Person")
}

func TestBuildRFC5322_MalformedTransportFallback(t *testing.T) {
	msg := &MessageEntry{
		TransportHeaders: "From: broken <\r\nTo: invalid <\r\nSubject: \r\n",
		SenderName:       "Sender", SenderEmail: "sender@example.test", Subject: "Recovered",
		Recipients: []MessageRecipient{{Name: "Recipient", Email: "recipient@example.test", Role: 1}},
	}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err)
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	from, err := message.Header.AddressList("From")
	require.NoError(t, err)
	assert.Equal(t, "sender@example.test", from[0].Address)
	to, err := message.Header.AddressList("To")
	require.NoError(t, err)
	assert.Equal(t, "recipient@example.test", to[0].Address)
	assert.Equal(t, "Recovered", message.Header.Get("Subject"))
	assert.Equal(t, "broken <", message.Header.Get("X-Msgvault-Original-From"))
	assert.Equal(t, "invalid <", message.Header.Get("X-Msgvault-Original-To"))
}

func TestBuildRFC5322_SentOnBehalf(t *testing.T) {
	msg := &MessageEntry{SenderName: "Assistant", SenderEmail: "assistant@example.test", AuthorName: "Owner", AuthorEmail: "owner@example.test"}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err)
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	from, err := message.Header.AddressList("From")
	require.NoError(t, err)
	assert.Equal(t, "owner@example.test", from[0].Address)
	sender, err := message.Header.AddressList("Sender")
	require.NoError(t, err)
	assert.Equal(t, "assistant@example.test", sender[0].Address)

	msg.TransportHeaders = "From: owner@example.test\r\nSubject: Test\r\n"
	raw, err = BuildRFC5322(msg, nil)
	require.NoError(t, err)
	message, err = mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	sender, err = message.Header.AddressList("Sender")
	require.NoError(t, err)
	assert.Equal(t, "assistant@example.test", sender[0].Address)
}

func TestBuildRFC5322_WithAttachments(t *testing.T) {
	assert := assert.New(t)
	msg := &MessageEntry{
		EntryID:     "42",
		Subject:     "With attachment",
		BodyText:    "See attached.",
		SenderEmail: "sender@example.com",
	}
	attachments := []AttachmentEntry{
		{
			Filename: "report.pdf",
			MIMEType: "application/pdf",
			Content:  []byte("%PDF-1.4 test"),
		},
	}

	raw, err := BuildRFC5322(msg, attachments)
	require.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	assert.Contains(s, "multipart/mixed", "expected multipart/mixed for message with attachments")
	assert.Contains(s, "report.pdf", "attachment filename not found")
	assert.Contains(s, "application/pdf", "attachment content type not found")
}

func TestBuildRFC5322_EmptyBody(t *testing.T) {
	msg := &MessageEntry{
		EntryID:     "1",
		SenderEmail: "a@b.com",
		Subject:     "No body",
	}

	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	assert.Contains(t, s, "text/plain", "expected text/plain even for empty body")
}

func TestSanitizeHeaderValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"normal@example.com", "normal@example.com"},
		{"evil@example.com\r\nBcc: victim@evil.com", "evil@example.comBcc: victim@evil.com"},
		{"has\nnewline", "hasnewline"},
		{"has\rreturn", "hasreturn"},
	}
	for _, tt := range tests {
		got := sanitizeHeaderValue(tt.in)
		assert.Equal(t, tt.want, got, "sanitizeHeaderValue(%q)", tt.in)
	}
}

func TestBuildRFC5322_HeaderInjection(t *testing.T) {
	msg := &MessageEntry{
		EntryID:     "1",
		SenderEmail: "evil@example.com\r\nBcc: victim@evil.com",
		Subject:     "Test",
		BodyText:    "body",
	}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err, "BuildRFC5322")
	// Check that "Bcc:" does not appear as a separate header line (the actual
	// injection vector). A sanitized value may still contain "Bcc:" as a
	// substring within the From address, but not as a new header line.
	assert.NotContains(t, string(raw), "\r\nBcc:", "header injection: Bcc header was injected via SenderEmail")
}

func TestBuildRFC5322_SanitizesMAPIHeaderValues(t *testing.T) {
	msg := &MessageEntry{
		SenderName: "Sender\r\nBcc: hidden@example.test", SenderEmail: "sender@example.test",
		Subject: "Subject\r\nBcc: injected@example.test", DisplayTo: "Alice\r\nBcc: injected@example.test",
	}
	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "\r\nBcc:")
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\evil\payload.exe`, "payload.exe"},
		{"file\x00name.txt", "filename.txt"},
		{"normal.doc", "normal.doc"},
		{"", ""},
	}
	for _, tt := range tests {
		got := sanitizeFilename(tt.in)
		assert.Equal(t, tt.want, got, "sanitizeFilename(%q)", tt.in)
	}
}

func TestSanitizeContentID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"abc123@example.com", "abc123@example.com"},
		{"<injected>header\r\n", "injectedheader"},
	}
	for _, tt := range tests {
		got := sanitizeContentID(tt.in)
		assert.Equal(t, tt.want, got, "sanitizeContentID(%q)", tt.in)
	}
}

func TestWriteQP_TrailingSpace(t *testing.T) {
	var buf bytes.Buffer
	writeQP(&buf, "hello \nworld")
	got := buf.String()
	assert.Contains(t, got, "hello=20\r\n", "trailing space not encoded: got %q", got)
}

func TestBuildRFC5322_TransportHeadersStripMIME(t *testing.T) {
	assert := assert.New(t)
	// Transport headers that include MIME headers — these should be stripped.
	transportHeaders := "From: alice@example.com\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=us-ascii\r\nContent-Transfer-Encoding: 7bit\r\nSubject: Old MIME\r\n"

	msg := &MessageEntry{
		TransportHeaders: transportHeaders,
		BodyText:         "Hello.",
	}

	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	// From and Subject should be present.
	assert.Contains(s, "From: alice@example.com", "From header missing")
	assert.Contains(s, "Subject: Old MIME", "Subject header missing")
	// The old Content-Type from transport headers should not appear verbatim.
	// (Our rebuilt MIME-Version and Content-Type replaces it.)
	// We expect exactly one Content-Type occurrence (ours, for text/plain).
	count := strings.Count(s, "Content-Type:")
	assert.Equal(1, count, "expected 1 Content-Type header")
}

func TestBuildRFC5322_MAPIThreadingFallback(t *testing.T) {
	for _, tc := range []struct{ name, headers, wantID, wantParent, wantRefs string }{
		{"partial", "From: sender@example.test\r\nSubject: Thread\r\n", "<mapi@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"},
		{"conflicts", "mEsSaGe-iD: <original@example.test>\r\nIN-REPLY-TO: <original-parent@example.test>\r\nReferences: <original-root@example.test>\r\n", "<original@example.test>", "<original-parent@example.test>", "<original-root@example.test>"},
		{"empty", "Message-ID: \t\r\nIn-Reply-To:\r\nReferences: \r\n", "<mapi@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"},
		{"folded", "Message-ID:\r\n\t<folded@example.test>\r\nReferences: <original-root@example.test>\r\n <original-parent@example.test>\r\n", "<folded@example.test>", "<parent@example.test>", "<original-root@example.test> <original-parent@example.test>"},
		{"separator", "From: sender@example.test\r\n\r\nMessage-ID: <body@example.test>\r\n", "<mapi@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"},
		{"no transport", "", "<mapi@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"},
	} {
		for _, layout := range []string{"plain", "html", "alternative", "attachment"} {
			t.Run(tc.name+"/"+layout, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				entry := &MessageEntry{TransportHeaders: tc.headers, SenderEmail: "sender@example.test", MessageID: " mapi@example.test ", InReplyTo: " parent@example.test ", References: "<root@example.test> <parent@example.test>", BodyText: "Body"}
				var attachments []AttachmentEntry
				if layout == "html" {
					entry.BodyText = ""
					entry.BodyHTML = "<p>Body</p>"
				}
				if layout == "alternative" {
					entry.BodyHTML = "<p>Body</p>"
				}
				if layout == "attachment" {
					attachments = []AttachmentEntry{{Filename: "example.txt", Content: []byte("Attachment")}}
				}
				raw, err := BuildRFC5322(entry, attachments)
				require.NoError(err)
				parsed, err := mail.ReadMessage(bytes.NewReader(raw))
				require.NoError(err)
				assert.Equal(tc.wantID, strings.TrimSpace(parsed.Header.Get("Message-ID")))
				assert.Equal(tc.wantParent, strings.TrimSpace(parsed.Header.Get("In-Reply-To")))
				assert.Equal(tc.wantRefs, strings.TrimSpace(parsed.Header.Get("References")))
				for _, field := range []string{"Message-Id", "In-Reply-To", "References"} {
					assert.Len(parsed.Header[field], 1, field)
				}
				full, err := msgmime.Parse(raw)
				require.NoError(err)
				assert.Equal("Body", strings.TrimSpace(full.GetBodyText()))
			})
		}
	}
}

func TestBuildRFC5322_ThreadingHeaderPrecedenceProperty(t *testing.T) {
	err := quick.Check(func(mapiBytes, originalBytes []byte, useOriginal bool) bool {
		mapiID := "mapi-" + hex.EncodeToString(mapiBytes) + "@example.test"
		originalID := "original-" + hex.EncodeToString(originalBytes) + "@example.test"
		headers := "From: sender@example.test\r\n"
		want := "<" + mapiID + ">"
		if useOriginal {
			headers += "mEsSaGe-ID:\r\n\t<" + originalID + ">\r\n"
			want = "<" + originalID + ">"
		}
		raw, err := BuildRFC5322(&MessageEntry{TransportHeaders: headers, MessageID: mapiID, BodyText: "Body"}, nil)
		if err != nil {
			return false
		}
		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		return err == nil && strings.TrimSpace(parsed.Header.Get("Message-ID")) == want && len(parsed.Header["Message-Id"]) == 1
	}, nil)
	require.NoError(t, err)
}

func TestBuildRFC5322_ThreadingFallbackRejectsHeaderInjection(t *testing.T) {
	for _, headers := range []string{"", "From: sender@example.test\r\n"} {
		raw, err := BuildRFC5322(&MessageEntry{TransportHeaders: headers, MessageID: "id@example.test\r\nBcc: injected@example.test", InReplyTo: "parent@example.test\nBcc: injected@example.test", References: "<root@example.test>\r\nBcc: injected@example.test", BodyText: "Body"}, nil)
		require.NoError(t, err)
		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		require.NoError(t, err)
		assert.Empty(t, parsed.Header.Get("Bcc"))
	}
}
