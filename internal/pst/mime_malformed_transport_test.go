package pst

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMalformedTransportHeadersRemainRecoverableAndMIMEUsable(t *testing.T) {
	for _, tc := range []struct{ name, headers string }{
		{"leading non-header and data after blank", "broken leading line\r\nFrom: transport@example.test\r\nSubject: Source\r\n\r\nnot a header: retained\r\n"},
		{"orphan continuation", "\torphan\r\nFrom: transport@example.test\r\nSubject: Source\r\n"},
		{"invalid field name", "bad name: value\r\nFrom: transport@example.test\r\nSubject: Source\r\n"},
		{"control in field", "Bad: prefix\x00suffix\r\nFrom: transport@example.test\r\nSubject: Source\r\n"},
		{"bad folded field", "Bad: value\r\n\tcontrol\x00\r\nFrom: transport@example.test\r\nSubject: Source\r\n"},
		{"bare CR injection", "Bad: value\rBcc: injected@example.test\r\nFrom: transport@example.test\r\nSubject: Source\r\n"},
		{"bare LF", "From: transport@example.test\nSubject: Source\n"},
		{"transport metadata collision", "broken\r\nX-Msgvault-Original-Transport-Headers: forged\r\nFrom: transport@example.test\r\nSubject: Source\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := &MessageEntry{TransportHeaders: tc.headers, SenderEmail: "fallback@example.test", BodyText: "Searchable body"}
			original := []byte{0, 1, 2, 0xff, 0x80}
			raw, err := BuildRFC5322(entry, []AttachmentEntry{{Filename: "original.bin", MIMEType: "application/octet-stream", Content: original}})
			require.NoError(t, err)
			m, err := mail.ReadMessage(bytes.NewReader(raw))
			require.NoError(t, err)
			assert.Equal(t, "transport@example.test", m.Header.Get("From"))
			assert.Equal(t, "Source", m.Header.Get("Subject"))
			assert.Empty(t, m.Header.Get("Bcc"))
			encoded := m.Header["X-Msgvault-Original-Transport-Headers"]
			require.Len(t, encoded, 1)
			restored, err := base64.RawURLEncoding.DecodeString(strings.Join(strings.Fields(encoded[0]), ""))
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.headers), restored)
			mediaType, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
			require.NoError(t, err)
			require.Equal(t, "multipart/mixed", mediaType)
			mr := multipart.NewReader(m.Body, params["boundary"])
			body, err := mr.NextPart()
			require.NoError(t, err)
			text, err := io.ReadAll(body)
			require.NoError(t, err)
			assert.Contains(t, string(text), "Searchable body")
			attachment, err := mr.NextPart()
			require.NoError(t, err)
			assert.Equal(t, "original.bin", attachment.FileName())
			decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, attachment))
			require.NoError(t, err)
			assert.Equal(t, original, decoded)
		})
	}
}

func TestMalformedTransportDoesNotInventSender(t *testing.T) {
	raw, err := BuildRFC5322(&MessageEntry{TransportHeaders: "not a field\r\nSubject: Original\r\n", BodyText: "body"}, nil)
	require.NoError(t, err)
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Empty(t, msg.Header.Get("From"))
	assert.Equal(t, "Original", msg.Header.Get("Subject"))
	assert.NotEmpty(t, msg.Header.Get("X-Msgvault-Original-Transport-Headers"))
}

func TestMalformedTransportKeepsSourceDateOrUsesMAPI(t *testing.T) {
	sent := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct{ name, date, want string }{
		{"valid transport date", "Date: Fri, 03 Jan 2020 03:04:05 +0000\r\n", "Fri, 03 Jan 2020 03:04:05 +0000"},
		{"missing transport date", "", "Thu, 02 Jan 2020 03:04:05 +0000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := BuildRFC5322(&MessageEntry{TransportHeaders: "invalid preamble\r\n" + tc.date, SentAt: sent}, nil)
			require.NoError(t, err)
			msg, err := mail.ReadMessage(bytes.NewReader(raw))
			require.NoError(t, err)
			assert.Equal(t, tc.want, msg.Header.Get("Date"))
		})
	}
}
